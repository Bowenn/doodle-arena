package com.doodlearena.marketplace.tip;

import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.UUID;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Isolation;
import org.springframework.transaction.annotation.Transactional;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.InsufficientFundsException;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.PostResult;
import com.doodlearena.marketplace.ledger.Posting;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * Takes a tip (ADR 0021): the tipper's coins go into one of the escrow shards at once, in one
 * ledger posting that locks only the tipper's balance and that shard, never the drawer's. Who drew
 * the round is decided later by its RoundFinished ({@link TipSettlement}), not by the client.
 */
@Service
public class TipService {

	private static final Logger log = LoggerFactory.getLogger(TipService.class);

	public static final long MAX_AMOUNT = 1_000;

	private final JdbcClient db;

	private final LedgerService ledger;

	private final MarketMetrics metrics;

	private final Duration roundOverAfter;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	public TipService(JdbcClient db, LedgerService ledger, MarketMetrics metrics,
			@Value("${app.tips.round-over-after}") Duration roundOverAfter) {
		this.db = db;
		this.ledger = ledger;
		this.metrics = metrics;
		this.roundOverAfter = roundOverAfter;
	}

	/** The shard a tip waits in: spread by its id, so one busy round uses every shard. */
	static int shardOf(UUID tipId) {
		return Math.floorMod(tipId.hashCode(), AccountRef.TIPS_ESCROW_SHARDS);
	}

	@Transactional(isolation = Isolation.READ_COMMITTED)
	@SuppressWarnings("try") // the scope only has to be open while the statements run
	public Tip tip(String tipperId, String roomId, String matchId, int roundNo, long amount) {
		Span span = this.tracer.spanBuilder("market.tip")
			.setAttribute("room.id", roomId)
			.setAttribute("match.id", matchId)
			.setAttribute("round.no", roundNo)
			.setAttribute("tip.amount", amount)
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			// A plain read: settlers lock the round's row, tippers never do. A tip that commits just
			// after its round was settled stays pending and the sweeper pays it.
			Optional<Instant> settledAt = this.db.sql("SELECT created_at FROM tip_rounds WHERE match_id = ? AND round_no = ?")
				.params(matchId, roundNo)
				.query(Instant.class)
				.optional();
			if (settledAt.isPresent() && settledAt.get().isBefore(Instant.now().minus(this.roundOverAfter))) {
				this.metrics.tip("round_over");
				throw new ApiException(HttpStatus.CONFLICT, "ROUND_OVER", "this round has ended");
			}

			UUID tipId = this.db.sql("SELECT uuidv7()").query(UUID.class).single();
			int shard = shardOf(tipId);
			span.setAttribute("tip.shard", shard);
			PostResult posted;
			try {
				posted = this.ledger.post(TransactionType.TIP, "tip:" + tipId,
						Map.of("tip_id", tipId.toString(), "room_id", roomId, "match_id", matchId, "round_no",
								Integer.toString(roundNo)),
						List.of(Posting.debit(AccountRef.user(tipperId), amount),
								Posting.credit(AccountRef.tipsEscrow(shard), amount)));
			}
			catch (InsufficientFundsException ex) {
				this.metrics.tip("insufficient_funds");
				throw new ApiException(HttpStatus.UNPROCESSABLE_CONTENT, "INSUFFICIENT_FUNDS",
						"a tip of " + amount + " coins needs that many in your wallet");
			}
			Tip tip = this.db.sql("INSERT INTO tips (tip_id, tipper_id, room_id, match_id, round_no, amount, shard, "
					+ "status, tx_id) VALUES (?, ?, ?, ?, ?, ?, ?, 'PENDING', ?) RETURNING " + Tip.COLUMNS)
				.params(tipId, tipperId, roomId, matchId, roundNo, amount, shard, posted.txId())
				.query(Tip.class)
				.single();
			TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
				@Override
				public void afterCommit() {
					TipService.this.metrics.tip("accepted");
				}
			});
			log.atInfo()
				.addKeyValue("user_id", tipperId)
				.addKeyValue("room_id", roomId)
				.addKeyValue("match_id", matchId)
				.addKeyValue("round_no", roundNo)
				.addKeyValue("tip_id", tipId)
				.addKeyValue("amount", amount)
				.log("tip taken into escrow");
			return tip;
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

}
