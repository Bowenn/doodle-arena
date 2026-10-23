package com.doodlearena.marketplace.drop;

import java.security.SecureRandom;
import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.function.Supplier;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.api.RateLimitedException;
import com.doodlearena.marketplace.idempotency.IdempotencyService;
import com.doodlearena.marketplace.idempotency.IdempotentResponse;
import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.InsufficientFundsException;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.PostResult;
import com.doodlearena.marketplace.ledger.Posting;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * Limited drops (ADR 0022): N numbered editions of an approved drawing, sold from a set time, one
 * per buyer, through a waiting room.
 *
 * <p>A purchase passes three gates. The user's token bucket and a read-only look at the drop in
 * Redis turn most of a burst away without touching Postgres. Then, under an Idempotency-Key, Redis
 * reserves one edition, and one Postgres transaction records it and moves the buyer's coins into
 * one of 16 escrow accounts. Postgres's constraints make overselling impossible whatever Redis
 * says. A purchase that doesn't commit gives its edition back in Redis.
 */
@Service
public class DropService {

	private static final Logger log = LoggerFactory.getLogger(DropService.class);

	public static final int MAX_EDITIONS = 1_000;

	public static final long MAX_PRICE = 100_000;

	static final Duration MIN_LEAD = Duration.ofSeconds(30);

	static final Duration MAX_LEAD = Duration.ofDays(30);

	public static final int MAX_DURATION_MINUTES = 24 * 60;

	private final JdbcClient db;

	private final LedgerService ledger;

	private final DropGate gate;

	private final IdempotencyService idempotency;

	private final MarketMetrics metrics;

	private final Clock clock;

	private final int admitPerSecond;

	private final int ratePerSecond;

	private final int rateBurst;

	private final SecureRandom random = new SecureRandom();

	/** Drops never change after they were created, so a lookup is a map read, not a query. */
	private final Map<UUID, Drop> drops = new ConcurrentHashMap<>();

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	public DropService(JdbcClient db, LedgerService ledger, DropGate gate, IdempotencyService idempotency,
			MarketMetrics metrics, Clock clock, @Value("${app.drops.admit-per-second}") int admitPerSecond,
			@Value("${app.drops.rate-limit-per-second}") int ratePerSecond,
			@Value("${app.drops.rate-limit-burst}") int rateBurst) {
		this.db = db;
		this.ledger = ledger;
		this.gate = gate;
		this.idempotency = idempotency;
		this.metrics = metrics;
		this.clock = clock;
		this.admitPerSecond = admitPerSecond;
		this.ratePerSecond = ratePerSecond;
		this.rateBurst = rateBurst;
	}

	/** An admin's new drop of a minted drawing, stocked in Redis in the same transaction. */
	@Transactional
	public DropView create(String adminId, UUID drawingId, int editions, long price, Instant opensAt,
			int durationMinutes) {
		Instant now = this.clock.instant();
		if (opensAt.isBefore(now.plus(MIN_LEAD)) || opensAt.isAfter(now.plus(MAX_LEAD))) {
			throw ApiException.invalidRequest("opens_at must be between 30 seconds and 30 days from now");
		}
		boolean minted = this.db.sql("SELECT EXISTS (SELECT 1 FROM ownership WHERE drawing_id = ?)")
			.param(drawingId)
			.query(Boolean.class)
			.single();
		if (!minted) {
			throw new ApiException(HttpStatus.NOT_FOUND, "DRAWING_NOT_FOUND", "no approved drawing has this id");
		}
		Drop drop = insert(adminId, drawingId, editions, price, opensAt,
				opensAt.plus(Duration.ofMinutes(durationMinutes)), this.admitPerSecond, now);
		log.atInfo()
			.addKeyValue("user_id", adminId)
			.addKeyValue("drawing_id", drawingId)
			.addKeyValue("drop_id", drop.dropId())
			.addKeyValue("editions", editions)
			.addKeyValue("price", price)
			.log("drop created");
		return view(drop, editions, now);
	}

	/**
	 * Stores and stocks a drop with no checks on its times (create checks them; tests open drops
	 * in the past). Inside the caller's transaction: if Redis can't stock it, it isn't stored.
	 */
	Drop insert(String adminId, UUID drawingId, int editions, long price, Instant opensAt, Instant closesAt,
			int admitPerSecond, Instant now) {
		Drop drop = this.db.sql("INSERT INTO drops (drawing_id, editions, price, opens_at, closes_at, "
				+ "admit_per_second, created_by) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING " + Drop.COLUMNS)
			.params(drawingId, editions, price, java.sql.Timestamp.from(opensAt), java.sql.Timestamp.from(closesAt),
					admitPerSecond, adminId)
			.query(Drop.class)
			.single();
		unavailable(() -> this.gate.stock(drop, now));
		this.drops.put(drop.dropId(), drop);
		return drop;
	}

	/** The drop, from the cache or Postgres; 404 for an unknown one. */
	public Drop drop(UUID dropId) {
		Drop cached = this.drops.get(dropId);
		if (cached != null) {
			return cached;
		}
		Drop drop = this.db.sql("SELECT " + Drop.COLUMNS + " FROM drops WHERE drop_id = ?")
			.param(dropId)
			.query(Drop.class)
			.optional()
			.orElseThrow(() -> new ApiException(HttpStatus.NOT_FOUND, "DROP_NOT_FOUND", "no drop has this id"));
		this.drops.put(dropId, drop);
		return drop;
	}

	/** One drop, its editions left from Redis, or from Postgres's sales when Redis can't answer. */
	public DropView get(UUID dropId) {
		Drop drop = drop(dropId);
		long left;
		try {
			left = this.gate.editionsLeft(drop);
		}
		catch (DropsUnavailableException ex) {
			left = drop.editions() - sold(dropId);
		}
		return view(drop, left, this.clock.instant());
	}

	/** Drops that are upcoming, open, or closed within the last day, soonest first. */
	public List<DropView> list() {
		Instant now = this.clock.instant();
		record Row(Drop drop, long sold) {
		}
		return this.db.sql("SELECT " + Drop.COLUMNS.replace("drop_id", "d.drop_id") + ", "
				+ "(SELECT COUNT(*) FROM drop_editions e WHERE e.drop_id = d.drop_id) AS sold "
				+ "FROM drops d WHERE closes_at > now() - interval '1 day' ORDER BY opens_at LIMIT 50")
			.query((rs, i) -> new Row(new Drop(rs.getObject(1, UUID.class), rs.getObject(2, UUID.class), rs.getInt(3),
					rs.getLong(4), rs.getTimestamp(5).toInstant(), rs.getTimestamp(6).toInstant(), rs.getInt(7),
					rs.getTimestamp(8).toInstant()), rs.getLong(9)))
			.list()
			.stream()
			.map((row) -> view(row.drop(), row.drop().editions() - row.sold(), now))
			.toList();
	}

	private long sold(UUID dropId) {
		return this.db.sql("SELECT COUNT(*) FROM drop_editions WHERE drop_id = ?").param(dropId).query(Long.class).single();
	}

	private static DropView view(Drop drop, long left, Instant now) {
		String phase = drop.phase(now);
		String status = (phase.equals("OPEN") && left <= 0) ? "SOLD_OUT" : phase;
		return new DropView(drop.dropId(), drop.drawingId(), drop.editions(), Math.max(0, left), drop.price(),
				drop.opensAt(), drop.closesAt(), status);
	}

	/**
	 * Joins the waiting room. Before the open, the place is a lottery: a random number decides it
	 * when the drop opens, so joining first or refreshing gives no advantage. After the open, later
	 * joiners queue behind everyone else in arrival order.
	 */
	public QueueStatus join(String user, UUID dropId) {
		Drop drop = drop(dropId);
		Instant now = this.clock.instant();
		rateLimit(user, now);
		if (!now.isBefore(drop.closesAt())) {
			throw closed();
		}
		double score = now.isBefore(drop.opensAt()) ? this.random.nextDouble() : 1 + now.toEpochMilli();
		unavailable(() -> this.gate.join(drop, user, score, now));
		this.metrics.dropJoin();
		return status(drop, user, now);
	}

	/** Where the caller stands. */
	public QueueStatus queue(String user, UUID dropId) {
		Drop drop = drop(dropId);
		Instant now = this.clock.instant();
		rateLimit(user, now);
		return status(drop, user, now);
	}

	private QueueStatus status(Drop drop, String user, Instant now) {
		return unavailable(() -> {
			Long rank = this.gate.rank(drop, user);
			long admitted = DropGate.admitted(drop, now);
			Long position = (rank == null) ? null : Math.max(0, rank - admitted + 1);
			long left = this.gate.editionsLeft(drop);
			DropView view = view(drop, left, now);
			return new QueueStatus(drop.dropId(), position, rank != null && rank < admitted, this.gate.waiting(drop),
					view.editionsLeft(), drop.opensAt(), view.status());
		});
	}

	/** How a purchase's Redis reservation ends if its transaction doesn't commit. */
	private static final class Reservation {

		final int edition;

		/** Postgres holds the edition for someone else: don't give the number back. */
		boolean forget;

		/**
		 * The edition Postgres already holds for this buyer (Redis forgot they bought): the new
		 * reservation goes back, and Redis learns the edition they own.
		 */
		Integer owned;

		Reservation(int edition) {
			this.edition = edition;
		}

	}

	/**
	 * Buys one edition. Refusals that change nothing (not open, not admitted, sold out) are answered
	 * before the Idempotency-Key is claimed; the reservation runs after the claim, outside the
	 * transaction, and the transaction that completes the key records the sale.
	 */
	@SuppressWarnings("try") // the scope only has to be open while the calls run
	public IdempotentResponse purchase(String buyer, UUID dropId, String key, String requestHash, String requestId) {
		Drop drop = drop(dropId);
		Span span = this.tracer.spanBuilder("market.drop.purchase")
			.setAttribute("drop.id", dropId.toString())
			.setAttribute("drawing.id", drop.drawingId().toString())
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			Instant now = this.clock.instant();
			rateLimit(buyer, now);
			refuseByTime(drop, now);
			DropGate.Answer look = unavailable(() -> this.gate.peek(drop, buyer, now));
			refuse(look);
			IdempotentResponse response = this.idempotency.execute(buyer, key, "drop_purchase", requestHash, requestId,
					HttpStatus.CREATED, (claim) -> reserve(drop, buyer), (claim, reservation) -> buy(drop, buyer, reservation));
			span.setAttribute("http.response.status_code", response.status());
			return response;
		}
		catch (RuntimeException ex) {
			span.recordException(ex);
			span.setStatus(StatusCode.ERROR);
			throw ex;
		}
		finally {
			span.end();
		}
	}

	private Reservation reserve(Drop drop, String buyer) {
		DropGate.Answer answer = unavailable(() -> this.gate.reserve(drop, buyer, this.clock.instant()));
		if (answer.outcome() == DropGate.Outcome.ALREADY_BOUGHT) {
			throw alreadyBought();
		}
		refuse(answer);
		return new Reservation(answer.value());
	}

	/** The sale itself, inside the transaction that completes the Idempotency-Key. */
	private Edition buy(Drop drop, String buyer, Reservation reservation) {
		TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
			@Override
			public void afterCompletion(int status) {
				settleReservation(drop, buyer, reservation, status == STATUS_COMMITTED);
			}
		});
		// Postgres's clock is the last word on the window: a reservation made just before the close
		// that reaches here after it doesn't sell.
		Boolean open = this.db.sql("SELECT now() >= opens_at AND now() < closes_at FROM drops WHERE drop_id = ?")
			.param(drop.dropId())
			.query(Boolean.class)
			.single();
		if (!Boolean.TRUE.equals(open)) {
			this.metrics.dropRequest("closed");
			throw closed();
		}
		int edition = reservation.edition;
		int shard = (edition - 1) % AccountRef.DROP_ESCROW_SHARDS;
		PostResult paid;
		try {
			paid = this.ledger.post(TransactionType.DROP_PURCHASE, "drop:" + drop.dropId() + ":" + buyer,
					Map.of("drop_id", drop.dropId().toString(), "drawing_id", drop.drawingId().toString(), "edition_no",
							Integer.toString(edition)),
					List.of(Posting.debit(AccountRef.user(buyer), drop.price()),
							Posting.credit(AccountRef.dropEscrow(shard), drop.price())));
		}
		catch (InsufficientFundsException ex) {
			this.metrics.dropRequest("insufficient_funds");
			throw new ApiException(HttpStatus.UNPROCESSABLE_CONTENT, "INSUFFICIENT_FUNDS",
					"an edition costs " + drop.price() + " coins");
		}
		if (paid.replayed()) {
			// The ledger already has this buyer's purchase (one per buyer): Redis forgot it.
			reservation.owned = this.db.sql("SELECT edition_no FROM drop_editions WHERE drop_id = ? AND buyer_id = ?")
				.params(drop.dropId(), buyer)
				.query(Integer.class)
				.optional()
				.orElse(null);
			throw alreadyBought();
		}
		Edition bought;
		try {
			bought = this.db.sql("""
					INSERT INTO drop_editions (drop_id, edition_no, editions, price, buyer_id, shard, status, tx_id)
					VALUES (?, ?, ?, ?, ?, ?, 'PAID', ?)
					RETURNING drop_id, ?::uuid AS drawing_id, edition_no, editions, price, purchased_at""")
				.params(drop.dropId(), edition, drop.editions(), drop.price(), buyer, shard, paid.txId(),
						drop.drawingId())
				.query(Edition.class)
				.single();
		}
		catch (DuplicateKeyException ex) {
			// Redis handed out an edition Postgres has already sold: the constraint refused it, and the
			// whole transaction (the coins too) rolls back. The number stays sold.
			reservation.forget = true;
			this.metrics.dropRequest("conflict");
			log.atWarn()
				.addKeyValue("user_id", buyer)
				.addKeyValue("drop_id", drop.dropId())
				.addKeyValue("edition_no", edition)
				.log("Redis reserved an edition Postgres already sold; refused");
			throw new ApiException(HttpStatus.CONFLICT, "DROP_CONFLICT", "that edition was taken: try again");
		}
		TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
			@Override
			public void afterCommit() {
				DropService.this.metrics.dropRequest("bought");
			}
		});
		log.atInfo()
			.addKeyValue("user_id", buyer)
			.addKeyValue("drawing_id", drop.drawingId())
			.addKeyValue("drop_id", drop.dropId())
			.addKeyValue("edition_no", edition)
			.addKeyValue("tx_id", paid.txId())
			.log("drop edition bought");
		return bought;
	}

	/**
	 * After the sale's transaction: a committed edition is marked paid in Redis; otherwise the
	 * reservation is given back (or forgotten). A Redis failure here only leaks a reservation, which
	 * the sweeper repairs; it never undoes the sale.
	 */
	private void settleReservation(Drop drop, String buyer, Reservation reservation, boolean committed) {
		try {
			if (committed) {
				this.gate.paid(drop, buyer, reservation.edition, this.clock.instant());
			}
			else if (reservation.owned != null) {
				// The edition just reserved goes back, unless it is the one they own (a retry of the
				// same purchase racing its own commit); then Redis records what they own.
				if (reservation.owned != reservation.edition) {
					this.gate.release(drop, buyer, reservation.edition);
				}
				this.gate.paid(drop, buyer, reservation.owned, this.clock.instant());
			}
			else if (reservation.forget) {
				this.gate.forget(drop, buyer, reservation.edition);
			}
			else {
				this.gate.release(drop, buyer, reservation.edition);
			}
		}
		catch (DropsUnavailableException ex) {
			log.atWarn()
				.addKeyValue("user_id", buyer)
				.addKeyValue("drop_id", drop.dropId())
				.addKeyValue("error", ex.getMessage())
				.log("reservation left as it was: the sweeper repairs it");
		}
	}

	/** The caller's editions, newest first. */
	public List<Edition> editions(String owner, int limit) {
		return this.db.sql("""
				SELECT e.drop_id, d.drawing_id, e.edition_no, e.editions, e.price, e.purchased_at
				FROM drop_editions e JOIN drops d USING (drop_id)
				WHERE e.buyer_id = ? ORDER BY e.purchased_at DESC LIMIT ?""")
			.params(owner, limit)
			.query(Edition.class)
			.list();
	}

	private void rateLimit(String user, Instant now) {
		long waitMs = unavailable(() -> this.gate.rateLimit(user, now, this.ratePerSecond, this.rateBurst));
		if (waitMs > 0) {
			this.metrics.dropRequest("rate_limited");
			throw new RateLimitedException(waitMs);
		}
	}

	private void refuseByTime(Drop drop, Instant now) {
		if (now.isBefore(drop.opensAt())) {
			this.metrics.dropRequest("not_open");
			throw new ApiException(HttpStatus.CONFLICT, "DROP_NOT_OPEN", "the drop opens at " + drop.opensAt());
		}
		if (!now.isBefore(drop.closesAt())) {
			this.metrics.dropRequest("closed");
			throw closed();
		}
	}

	/** Throws the refusal an answer means; returns for HELD, GO and RESERVED. */
	private void refuse(DropGate.Answer answer) {
		switch (answer.outcome()) {
			case NOT_JOINED -> {
				this.metrics.dropRequest("not_admitted");
				throw new ApiException(HttpStatus.FORBIDDEN, "NOT_ADMITTED", "join the waiting room first");
			}
			case NOT_ADMITTED -> {
				this.metrics.dropRequest("not_admitted");
				throw new ApiException(HttpStatus.FORBIDDEN, "NOT_ADMITTED",
						answer.value() + " places ahead of you in the waiting room");
			}
			case SOLD_OUT -> {
				this.metrics.dropRequest("sold_out");
				throw new ApiException(HttpStatus.CONFLICT, "SOLD_OUT", "every edition is taken");
			}
			case ALREADY_BOUGHT -> throw alreadyBought();
			default -> {
				// HELD, GO, RESERVED: go on.
			}
		}
	}

	private ApiException alreadyBought() {
		this.metrics.dropRequest("already_bought");
		return new ApiException(HttpStatus.CONFLICT, "ALREADY_BOUGHT", "one edition per buyer");
	}

	private static ApiException closed() {
		return new ApiException(HttpStatus.CONFLICT, "DROP_CLOSED", "the drop has closed");
	}

	private <T> T unavailable(Supplier<T> call) {
		try {
			return call.get();
		}
		catch (DropsUnavailableException ex) {
			this.metrics.dropRequest("unavailable");
			log.atWarn().addKeyValue("error", ex.getMessage()).log("drops unavailable");
			throw new ApiException(HttpStatus.SERVICE_UNAVAILABLE, "DROPS_UNAVAILABLE",
					"drops are unavailable right now: try again shortly");
		}
	}

}
