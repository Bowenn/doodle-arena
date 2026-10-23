package com.doodlearena.marketplace.drop;

import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import java.util.UUID;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Service;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.PostResult;
import com.doodlearena.marketplace.ledger.Posting;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.outbox.OutboxWriter;
import com.doodlearena.marketplace.telemetry.MarketMetrics;
import com.doodlearena.proto.doodle.events.v1.CoinsCredited;
import com.doodlearena.proto.doodle.events.v1.CreditReason;
import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.WalletEvent;

/**
 * Every 10 seconds (ADR 0022): pays sold-out or closed drops' editions to the drawing's authors,
 * gives back reservations whose purchase never committed, and restocks a drop whose inventory
 * Redis lost.
 *
 * <p>A payout is one ledger posting per batch: each escrow shard debited by its editions, the
 * authors credited, split like royalties. Only this sweeper locks a drop's row, which also counts
 * the settlements, so each posting has its own idempotency key.
 */
@Service
public class DropSettlement {

	private static final Logger log = LoggerFactory.getLogger(DropSettlement.class);

	/** Editions per posting: bounds the statement that closes them. */
	static final int BATCH = 5_000;

	private final JdbcClient db;

	private final LedgerService ledger;

	private final OutboxWriter outbox;

	private final DropGate gate;

	private final DropService drops;

	private final MarketMetrics metrics;

	private final Clock clock;

	private final Duration leakAfter;

	private final TransactionTemplate transaction;

	private final boolean sweeperEnabled;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	public DropSettlement(JdbcClient db, LedgerService ledger, OutboxWriter outbox, DropGate gate, DropService drops,
			MarketMetrics metrics, Clock clock, PlatformTransactionManager transactions,
			@Value("${app.drops.leak-after}") Duration leakAfter,
			@Value("${app.drops.sweeper-enabled:true}") boolean sweeperEnabled) {
		this.db = db;
		this.ledger = ledger;
		this.outbox = outbox;
		this.gate = gate;
		this.drops = drops;
		this.metrics = metrics;
		this.clock = clock;
		this.leakAfter = leakAfter;
		this.sweeperEnabled = sweeperEnabled;
		this.transaction = new TransactionTemplate(transactions);
		this.transaction.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
	}

	@Scheduled(fixedDelayString = "${app.drops.sweep-interval}")
	public void scheduledSweep() {
		if (this.sweeperEnabled) {
			sweep();
		}
	}

	/** One pass of the sweeper. */
	public void sweep() {
		Instant now = this.clock.instant();
		try {
			// Open drops: Redis's view repaired against Postgres.
			for (UUID dropId : this.db.sql("SELECT drop_id FROM drops WHERE opens_at <= now() AND closes_at > now()")
				.query(UUID.class)
				.list()) {
				Drop drop = this.drops.drop(dropId);
				try {
					restock(drop, now);
					repairLeaks(drop, now);
				}
				catch (DropsUnavailableException ex) {
					log.atWarn().addKeyValue("drop_id", dropId).addKeyValue("error", ex.getMessage()).log("drop repair skipped");
				}
			}
			// Sold out or closed, and not yet paid out.
			for (UUID dropId : this.db.sql("SELECT d.drop_id FROM drops d WHERE d.settled_at IS NULL AND d.opens_at <= now() AND "
					+ DUE)
				.query(UUID.class)
				.list()) {
				settle(dropId);
			}
		}
		catch (RuntimeException ex) {
			// The next run tries again.
			log.error("drop sweep failed", ex);
		}
	}

	/** Rebuilds the free list if Redis lost it (its stocked marker is gone). */
	void restock(Drop drop, Instant now) {
		if (this.gate.stocked(drop)) {
			return;
		}
		List<Integer> sold = this.db.sql("SELECT edition_no FROM drop_editions WHERE drop_id = ?")
			.param(drop.dropId())
			.query(Integer.class)
			.list();
		long back = this.gate.restock(drop, sold, now);
		log.atWarn()
			.addKeyValue("drop_id", drop.dropId())
			.addKeyValue("editions_back", back)
			.log("drop inventory missing in Redis; rebuilt from Postgres");
	}

	/**
	 * Reservations older than leak-after: marked paid if Postgres has the sale (the marketplace died
	 * before telling Redis), else given back.
	 */
	void repairLeaks(Drop drop, Instant now) {
		long released = 0;
		for (Map.Entry<String, String> entry : this.gate.buyers(drop).entrySet()) {
			String[] parts = entry.getValue().split(":");
			if (!parts[0].equals("r") || parts.length < 3) {
				continue;
			}
			int edition = Integer.parseInt(parts[1]);
			Instant reservedAt = Instant.ofEpochMilli(Long.parseLong(parts[2]));
			if (reservedAt.plus(this.leakAfter).isAfter(now)) {
				continue;
			}
			Integer held = this.db.sql("SELECT edition_no FROM drop_editions WHERE drop_id = ? AND buyer_id = ?")
				.params(drop.dropId(), entry.getKey())
				.query(Integer.class)
				.optional()
				.orElse(null);
			if (held != null) {
				this.gate.paid(drop, entry.getKey(), held, now);
			}
			else if (this.gate.release(drop, entry.getKey(), edition)) {
				released++;
			}
		}
		if (released > 0) {
			this.metrics.leakedReservations(released);
			log.atWarn().addKeyValue("drop_id", drop.dropId()).addKeyValue("released", released).log("leaked drop reservations given back");
		}
	}

	/**
	 * A drop is paid out once it sold out (nothing more can sell) or a minute after it closed: a
	 * purchase that passed its window check just before the close may still be committing.
	 */
	private static final String DUE = "(d.closes_at <= now() - interval '1 minute' "
			+ "OR (SELECT COUNT(*) FROM drop_editions e WHERE e.drop_id = d.drop_id) >= d.editions)";

	/** What a settlement paid. */
	public record Settled(int editions, long coins) {
	}

	/** Pays the drop's paid editions to its authors, a batch per transaction, then marks it settled. */
	@SuppressWarnings("try") // the scope only has to be open while the statements run
	public Settled settle(UUID dropId) {
		Span span = this.tracer.spanBuilder("market.drop.settle").setAttribute("drop.id", dropId.toString()).startSpan();
		try (Scope scope = span.makeCurrent()) {
			int editions = 0;
			long coins = 0;
			while (true) {
				Settled batch = this.transaction.execute((status) -> settleBatch(dropId));
				editions += batch.editions();
				coins += batch.coins();
				// The last pass finds nothing left and marks the drop settled.
				if (batch.editions() == 0) {
					span.setAttribute("drop.editions_settled", editions);
					return new Settled(editions, coins);
				}
			}
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

	private record Paid(int editionNo, long price, int shard) {
	}

	private Settled settleBatch(UUID dropId) {
		long started = System.nanoTime();
		record Row(UUID drawingId, int editions, int settlements, boolean due) {
		}
		Row drop = this.db.sql("SELECT drawing_id, editions, settlements, " + DUE
				+ " FROM drops d WHERE drop_id = ? AND settled_at IS NULL FOR UPDATE")
			.param(dropId)
			.query((rs, i) -> new Row(rs.getObject(1, UUID.class), rs.getInt(2), rs.getInt(3), rs.getBoolean(4)))
			.optional()
			.orElse(null);
		if (drop == null || !drop.due()) {
			return new Settled(0, 0);
		}
		List<Paid> paid = this.db.sql("""
				SELECT edition_no, price, shard FROM drop_editions
				WHERE drop_id = ? AND status = 'PAID' ORDER BY edition_no LIMIT ? FOR UPDATE""")
			.params(dropId, BATCH)
			.query((rs, i) -> new Paid(rs.getInt(1), rs.getLong(2), rs.getInt(3)))
			.list();
		this.metrics.recordLockWait("drop_settlement", System.nanoTime() - started);
		if (paid.isEmpty()) {
			// Nothing left in escrow, and nothing more can be sold: the drop is done.
			this.db.sql("UPDATE drops SET settled_at = now() WHERE drop_id = ?").param(dropId).update();
			log.atInfo().addKeyValue("drop_id", dropId).log("drop settled");
			return new Settled(0, 0);
		}
		List<String> authors = this.db.sql("SELECT user_id FROM drawing_authors WHERE drawing_id = ? ORDER BY author_order")
			.param(drop.drawingId())
			.query(String.class)
			.list();
		Map<Integer, Long> byShard = new TreeMap<>();
		long total = 0;
		for (Paid edition : paid) {
			byShard.merge(edition.shard(), edition.price(), Long::sum);
			total += edition.price();
		}
		List<Posting> postings = new ArrayList<>();
		byShard.forEach((shard, amount) -> postings.add(Posting.debit(AccountRef.dropEscrow(shard), amount)));
		Map<String, Long> shares = split(total, authors);
		shares.forEach((author, amount) -> postings.add(Posting.credit(AccountRef.user(author), amount)));
		int settlement = drop.settlements() + 1;
		PostResult posted = this.ledger.post(TransactionType.DROP_SETTLEMENT, "drop-settle:" + dropId + ":" + settlement,
				Map.of("drop_id", dropId.toString(), "drawing_id", drop.drawingId().toString(), "editions",
						Integer.toString(paid.size())),
				postings);
		if (posted.replayed()) {
			// The counter moves under the drop's lock in the same transaction, so a key is never reused.
			throw new IllegalStateException("settlement " + settlement + " of drop " + dropId + " already posted");
		}
		this.db.sql("UPDATE drops SET settlements = ? WHERE drop_id = ?").params(settlement, dropId).update();
		this.db.sql("""
				UPDATE drop_editions SET status = 'SETTLED', settle_tx_id = ?, settled_at = now()
				WHERE drop_id = ? AND edition_no = ANY(?)""")
			.params(posted.txId(), dropId, paid.stream().map(Paid::editionNo).toArray(Integer[]::new))
			.update();
		Instant now = Instant.now();
		shares.forEach((author, amount) -> {
			EventMeta meta = OutboxWriter.meta("CoinsCredited", now);
			this.outbox.write(OutboxWriter.WALLET_TOPIC, author, meta, WalletEvent.newBuilder()
				.setCoinsCredited(CoinsCredited.newBuilder()
					.setMeta(meta)
					.setUserId(author)
					.setAmount(amount)
					.setReason(CreditReason.CREDIT_REASON_DROP_SALE)
					.setReference(dropId.toString()))
				.build()
				.toByteArray());
		});
		int count = paid.size();
		TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
			@Override
			public void afterCommit() {
				DropSettlement.this.metrics.dropsSettled(count);
			}
		});
		log.atInfo()
			.addKeyValue("drop_id", dropId)
			.addKeyValue("drawing_id", drop.drawingId())
			.addKeyValue("tx_id", posted.txId())
			.addKeyValue("editions", count)
			.addKeyValue("coins", total)
			.log("drop sales paid to the authors");
		return new Settled(count, total);
	}

	/**
	 * The authors' shares, integer coins only: each gets {@code floor(total / n)} and the first the
	 * remainder, as royalties do. A share of 0 is left out (a ledger entry is never 0).
	 */
	static Map<String, Long> split(long total, List<String> authors) {
		Map<String, Long> shares = new java.util.LinkedHashMap<>();
		long each = total / authors.size();
		long remainder = total - each * authors.size();
		for (int i = 0; i < authors.size(); i++) {
			long share = each + ((i == 0) ? remainder : 0);
			if (share > 0) {
				shares.put(authors.get(i), share);
			}
		}
		return shares;
	}

}
