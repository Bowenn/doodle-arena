package com.doodlearena.marketplace.reward;

import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.PostResult;
import com.doodlearena.marketplace.ledger.Posting;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.outbox.OutboxWriter;
import com.doodlearena.marketplace.reward.RewardRequest.Placement;
import com.doodlearena.proto.doodle.events.v1.CoinsCredited;
import com.doodlearena.proto.doodle.events.v1.CreditReason;
import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.WalletEvent;

/**
 * Pays match rewards (design doc, section 7.9): coins by placement, from the rewards_issuance
 * account, in one ledger transaction per match with the key {@code reward:<match_id>}. A match is
 * paid all or nothing, the system account's balance row is locked once per match, and a
 * redelivered MatchFinished finds the transaction and pays nothing.
 */
@Service
public class RewardService {

	private static final Logger log = LoggerFactory.getLogger(RewardService.class);

	private final LedgerService ledger;

	private final OutboxWriter outbox;

	private final List<Long> placementCoins;

	private final long participationCoins;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	public RewardService(LedgerService ledger, OutboxWriter outbox,
			@Value("${app.rewards.placement-coins}") List<Long> placementCoins,
			@Value("${app.rewards.participation-coins}") long participationCoins) {
		this.ledger = ledger;
		this.outbox = outbox;
		this.placementCoins = List.copyOf(placementCoins);
		this.participationCoins = participationCoins;
	}

	/** A match needs this many players to pay anything: nobody plays a drawing game alone. */
	public static final int MIN_PLAYERS = 2;

	public enum RewardResult {

		PAID, DUPLICATE, INELIGIBLE

	}

	/** Coins for a placement: 1st, 2nd and 3rd from the list, everyone else the participation reward. */
	long coinsFor(int placement) {
		return (placement <= this.placementCoins.size()) ? this.placementCoins.get(placement - 1) : this.participationCoins;
	}

	@Transactional
	@SuppressWarnings("try") // the scope only has to be open while the statements run
	public RewardResult pay(RewardRequest request) {
		Span span = this.tracer.spanBuilder("market.reward")
			.setAttribute("match.id", request.matchId())
			.setAttribute("reward.players", request.placements().size())
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			if (request.placements().size() < MIN_PLAYERS) {
				span.setAttribute("reward.ineligible", true);
				log.atInfo().addKeyValue("match_id", request.matchId()).log("match not rewarded: fewer than two players");
				return RewardResult.INELIGIBLE;
			}
			List<Posting> postings = new ArrayList<>();
			long total = 0;
			for (Placement placement : request.placements()) {
				long coins = coinsFor(placement.placement());
				postings.add(Posting.credit(AccountRef.user(placement.userId()), coins));
				total += coins;
			}
			postings.add(Posting.debit(AccountRef.REWARDS_ISSUANCE, total));
			span.setAttribute("reward.coins", total);

			PostResult result = this.ledger.post(TransactionType.REWARD, "reward:" + request.matchId(),
					Map.of("match_id", request.matchId(), "room_id", request.roomId()), postings);
			if (result.replayed()) {
				span.setAttribute("reward.duplicate", true);
				log.atInfo().addKeyValue("match_id", request.matchId()).log("match already rewarded");
				return RewardResult.DUPLICATE;
			}
			Instant now = Instant.now();
			for (Placement placement : request.placements()) {
				EventMeta meta = OutboxWriter.meta("CoinsCredited", now);
				this.outbox.write(OutboxWriter.WALLET_TOPIC, placement.userId(), meta, WalletEvent.newBuilder()
					.setCoinsCredited(CoinsCredited.newBuilder()
						.setMeta(meta)
						.setUserId(placement.userId())
						.setAmount(coinsFor(placement.placement()))
						.setReason(CreditReason.CREDIT_REASON_REWARD)
						.setReference(request.matchId()))
					.build()
					.toByteArray());
			}
			log.atInfo()
				.addKeyValue("match_id", request.matchId())
				.addKeyValue("tx_id", result.txId())
				.addKeyValue("players", request.placements().size())
				.addKeyValue("coins", total)
				.log("match rewarded");
			return RewardResult.PAID;
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
