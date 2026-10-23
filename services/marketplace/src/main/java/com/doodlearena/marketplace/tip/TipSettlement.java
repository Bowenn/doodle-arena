package com.doodlearena.marketplace.tip;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicReference;

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
 * Takes tips out of escrow (ADR 0021). A round's RoundFinished says who drew it; its pending tips
 * are then paid to those drawers in one ledger posting per batch, which locks each drawer's
 * balance once rather than once per tip. A tip from one of the round's drawers, or naming another
 * room than the round's, goes back to its tipper in the same posting.
 *
 * <p>Only settlers lock a round's {@code tip_rounds} row, so tippers never wait for them; the row
 * also counts the settlements, which gives each posting its idempotency key. A redelivered
 * RoundFinished finds nothing pending and pays nothing. Every 10 seconds the sweeper settles tips
 * committed after their round was settled, and refunds tips whose round never finished.
 */
@Service
public class TipSettlement {

	private static final Logger log = LoggerFactory.getLogger(TipSettlement.class);

	/** Tips per posting: bounds the statement that closes them. A bigger round takes more postings. */
	static final int BATCH = 5_000;

	/** Stale tips refunded per sweeper transaction. */
	static final int REFUND_BATCH = 100;

	private final JdbcClient db;

	private final LedgerService ledger;

	private final OutboxWriter outbox;

	private final MarketMetrics metrics;

	private final TransactionTemplate transaction;

	private final Duration refundAfter;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	/** Pending tips and the oldest one's age in seconds, from the sweeper's last run. */
	private final AtomicReference<double[]> pending = new AtomicReference<>();

	public TipSettlement(JdbcClient db, LedgerService ledger, OutboxWriter outbox, MarketMetrics metrics,
			PlatformTransactionManager transactions, @Value("${app.tips.refund-after}") Duration refundAfter) {
		this.db = db;
		this.ledger = ledger;
		this.outbox = outbox;
		this.metrics = metrics;
		this.transaction = new TransactionTemplate(transactions);
		this.transaction.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
		this.refundAfter = refundAfter;
		metrics.meter()
			.gaugeBuilder("market.tips.pending")
			.ofLongs()
			.setUnit("{tip}")
			.setDescription("Tips in escrow, waiting for their round to finish")
			.buildWithCallback((gauge) -> {
				double[] last = this.pending.get();
				if (last != null) {
					gauge.record((long) last[0]);
				}
			});
		metrics.meter()
			.gaugeBuilder("market.tips.oldest_pending")
			.setUnit("s")
			.setDescription("Age of the oldest tip in escrow (0 when none)")
			.buildWithCallback((gauge) -> {
				double[] last = this.pending.get();
				if (last != null) {
					gauge.record(last[1]);
				}
			});
	}

	/** What one settlement closed. */
	public record Settled(int paid, int refunded, long coinsPaid) {

		static final Settled NONE = new Settled(0, 0, 0);

		Settled plus(Settled other) {
			return new Settled(this.paid + other.paid, this.refunded + other.refunded, this.coinsPaid + other.coinsPaid);
		}

	}

	private record Round(String roomId, List<String> drawerIds, int settlements) {
	}

	private record PendingTip(UUID tipId, String tipperId, String roomId, long amount, int shard) {
	}

	/** A RoundFinished arrived: remember who drew the round and pay its pending tips. */
	@SuppressWarnings("try") // the scope only has to be open while the statements run
	public Settled roundFinished(RoundRequest round) {
		Span span = this.tracer.spanBuilder("market.tip.settle")
			.setAttribute("match.id", round.matchId())
			.setAttribute("room.id", round.roomId())
			.setAttribute("round.no", round.roundNo())
			.setAttribute("tip.trigger", "round")
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			this.db.sql("""
					INSERT INTO tip_rounds (match_id, round_no, room_id, drawer_ids) VALUES (?, ?, ?, ?)
					ON CONFLICT (match_id, round_no) DO NOTHING""")
				.params(round.matchId(), round.roundNo(), round.roomId(), round.drawerIds().toArray(String[]::new))
				.update();
			Settled settled = settleAll(round.matchId(), round.roundNo(), "round");
			span.setAttribute("tip.paid", settled.paid());
			span.setAttribute("tip.refunded", settled.refunded());
			return settled;
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

	/** Settles a known round's pending tips, a batch per transaction, until none are left. */
	Settled settleAll(String matchId, int roundNo, String trigger) {
		Settled total = Settled.NONE;
		while (true) {
			Settled batch = this.transaction.execute((status) -> settleBatch(matchId, roundNo, trigger));
			total = total.plus(batch);
			if (batch.paid() + batch.refunded() < BATCH) {
				return total;
			}
		}
	}

	private Settled settleBatch(String matchId, int roundNo, String trigger) {
		long started = System.nanoTime();
		Round round = this.db.sql("""
				SELECT room_id, drawer_ids, settlements FROM tip_rounds WHERE match_id = ? AND round_no = ? FOR UPDATE""")
			.params(matchId, roundNo)
			.query((rs, i) -> new Round(rs.getString(1), List.of((String[]) rs.getArray(2).getArray()), rs.getInt(3)))
			.single();
		// FOR UPDATE: a tip the sweeper is refunding is skipped here once its status changed.
		List<PendingTip> tips = this.db.sql("""
				SELECT tip_id, tipper_id, room_id, amount, shard FROM tips
				WHERE match_id = ? AND round_no = ? AND status = 'PENDING'
				ORDER BY tip_id LIMIT ? FOR UPDATE""")
			.params(matchId, roundNo, BATCH)
			.query((rs, i) -> new PendingTip(rs.getObject(1, UUID.class), rs.getString(2), rs.getString(3),
					rs.getLong(4), rs.getInt(5)))
			.list();
		this.metrics.recordLockWait("tip_settlement", System.nanoTime() - started);
		if (tips.isEmpty()) {
			return Settled.NONE;
		}

		Map<Integer, Long> byShard = new TreeMap<>();
		List<PendingTip> paid = new ArrayList<>();
		List<PendingTip> refunded = new ArrayList<>();
		long paidTotal = 0;
		for (PendingTip tip : tips) {
			byShard.merge(tip.shard(), tip.amount(), Long::sum);
			// Nobody tips themselves, and a tip naming another room wasn't for these drawers.
			if (round.drawerIds().contains(tip.tipperId()) || !round.roomId().equals(tip.roomId())) {
				refunded.add(tip);
			}
			else {
				paid.add(tip);
				paidTotal += tip.amount();
			}
		}

		List<Posting> postings = new ArrayList<>();
		byShard.forEach((shard, amount) -> postings.add(Posting.debit(AccountRef.tipsEscrow(shard), amount)));
		Map<String, Long> shares = split(paidTotal, round.drawerIds());
		shares.forEach((drawer, amount) -> postings.add(Posting.credit(AccountRef.user(drawer), amount)));
		for (PendingTip tip : refunded) {
			postings.add(Posting.credit(AccountRef.user(tip.tipperId()), tip.amount()));
		}
		int settlement = round.settlements() + 1;
		PostResult posted = this.ledger.post(TransactionType.TIP_SETTLEMENT,
				"tip-settle:" + matchId + ":" + roundNo + ":" + settlement,
				Map.of("match_id", matchId, "round_no", Integer.toString(roundNo), "room_id", round.roomId(), "tips",
						Integer.toString(tips.size())),
				postings);
		if (posted.replayed()) {
			// The counter moves under the round's lock in the same transaction, so a key is never reused.
			throw new IllegalStateException("settlement " + settlement + " of round " + roundNo + " already posted");
		}
		this.db.sql("UPDATE tip_rounds SET settlements = ? WHERE match_id = ? AND round_no = ?")
			.params(settlement, matchId, roundNo)
			.update();
		close(paid, "SETTLED", posted.txId());
		close(refunded, "REFUNDED", posted.txId());

		Instant now = Instant.now();
		String reference = matchId + ":" + roundNo;
		shares.forEach((drawer, amount) -> credited(drawer, amount, CreditReason.CREDIT_REASON_TIP, reference, now));
		for (PendingTip tip : refunded) {
			credited(tip.tipperId(), tip.amount(), CreditReason.CREDIT_REASON_TIP_REFUND, tip.tipId().toString(), now);
		}
		long coins = paidTotal;
		afterCommit(() -> {
			this.metrics.tipsClosed(paid.size(), "paid", trigger);
			this.metrics.tipsClosed(refunded.size(), "refunded", trigger);
		});
		log.atInfo()
			.addKeyValue("match_id", matchId)
			.addKeyValue("room_id", round.roomId())
			.addKeyValue("round_no", roundNo)
			.addKeyValue("tx_id", posted.txId())
			.addKeyValue("paid", paid.size())
			.addKeyValue("refunded", refunded.size())
			.addKeyValue("coins", coins)
			.addKeyValue("trigger", trigger)
			.log("tips settled");
		return new Settled(paid.size(), refunded.size(), coins);
	}

	/**
	 * The drawers' shares, integer coins only: each gets {@code floor(total / n)} and the first the
	 * remainder, as royalties do. A share of 0 is left out (a ledger entry is never 0).
	 */
	static Map<String, Long> split(long total, List<String> drawers) {
		Map<String, Long> shares = new LinkedHashMap<>();
		long each = total / drawers.size();
		long remainder = total - each * drawers.size();
		for (int i = 0; i < drawers.size(); i++) {
			long share = each + ((i == 0) ? remainder : 0);
			if (share > 0) {
				shares.put(drawers.get(i), share);
			}
		}
		return shares;
	}

	/**
	 * Every 10 seconds: pay tips that committed after their round was settled (to the same drawers),
	 * refund tips whose round never finished, and measure what is left in escrow.
	 */
	@Scheduled(fixedDelayString = "${app.tips.sweep-interval}")
	public void sweep() {
		try {
			record RoundKey(String matchId, int roundNo) {
			}
			List<RoundKey> late = this.db.sql("""
					SELECT DISTINCT t.match_id, t.round_no FROM tips t
					JOIN tip_rounds r USING (match_id, round_no)
					WHERE t.status = 'PENDING'""")
				.query((rs, i) -> new RoundKey(rs.getString(1), rs.getInt(2)))
				.list();
			for (RoundKey round : late) {
				settleAll(round.matchId(), round.roundNo(), "sweep");
			}
			while (refundStale() == REFUND_BATCH) {
				// Another batch: a long outage of the game server can leave many.
			}
			this.pending.set(this.db.sql("""
					SELECT COUNT(*), COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)
					FROM tips WHERE status = 'PENDING'""")
				.query((rs, i) -> new double[] { rs.getLong(1), rs.getDouble(2) })
				.single());
		}
		catch (RuntimeException ex) {
			// The next run tries again; TipsStuck fires if tips keep waiting.
			log.error("tip sweep failed", ex);
		}
	}

	/**
	 * Refunds up to {@link #REFUND_BATCH} tips whose round never finished within refund-after: a
	 * match that ended while a word was being chosen publishes no RoundFinished, and a client can
	 * name a round that doesn't exist. One posting per tip, each with its own key.
	 * @return how many it refunded
	 */
	int refundStale() {
		Integer refunded = this.transaction.execute((status) -> {
			List<PendingTip> stale = this.db.sql("""
					SELECT tip_id, tipper_id, room_id, amount, shard FROM tips t
					WHERE status = 'PENDING' AND created_at < now() - make_interval(secs => ?)
						AND NOT EXISTS (SELECT 1 FROM tip_rounds r WHERE r.match_id = t.match_id AND r.round_no = t.round_no)
					ORDER BY created_at LIMIT ? FOR UPDATE SKIP LOCKED""")
				.params((double) this.refundAfter.toSeconds(), REFUND_BATCH)
				.query((rs, i) -> new PendingTip(rs.getObject(1, UUID.class), rs.getString(2), rs.getString(3),
						rs.getLong(4), rs.getInt(5)))
				.list();
			Instant now = Instant.now();
			for (PendingTip tip : stale) {
				PostResult posted = this.ledger.post(TransactionType.TIP_REFUND, "tip-refund:" + tip.tipId(),
						Map.of("tip_id", tip.tipId().toString()),
						List.of(Posting.debit(AccountRef.tipsEscrow(tip.shard()), tip.amount()),
								Posting.credit(AccountRef.user(tip.tipperId()), tip.amount())));
				close(List.of(tip), "REFUNDED", posted.txId());
				credited(tip.tipperId(), tip.amount(), CreditReason.CREDIT_REASON_TIP_REFUND, tip.tipId().toString(), now);
				log.atInfo()
					.addKeyValue("user_id", tip.tipperId())
					.addKeyValue("room_id", tip.roomId())
					.addKeyValue("tip_id", tip.tipId())
					.addKeyValue("amount", tip.amount())
					.log("tip refunded: its round never finished");
			}
			afterCommit(() -> this.metrics.tipsClosed(stale.size(), "refunded", "sweep"));
			return stale.size();
		});
		return (refunded == null) ? 0 : refunded;
	}

	private void close(List<PendingTip> tips, String status, UUID closingTxId) {
		if (tips.isEmpty()) {
			return;
		}
		this.db.sql("UPDATE tips SET status = ?, closing_tx_id = ?, closed_at = now() WHERE tip_id = ANY(?)")
			.params(status, closingTxId, tips.stream().map(PendingTip::tipId).toArray(UUID[]::new))
			.update();
	}

	private void credited(String userId, long amount, CreditReason reason, String reference, Instant now) {
		EventMeta meta = OutboxWriter.meta("CoinsCredited", now);
		this.outbox.write(OutboxWriter.WALLET_TOPIC, userId, meta, WalletEvent.newBuilder()
			.setCoinsCredited(CoinsCredited.newBuilder()
				.setMeta(meta)
				.setUserId(userId)
				.setAmount(amount)
				.setReason(reason)
				.setReference(reference))
			.build()
			.toByteArray());
	}

	private static void afterCommit(Runnable action) {
		TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
			@Override
			public void afterCommit() {
				action.run();
			}
		});
	}

}
