package com.doodlearena.marketplace.tip;

import java.util.List;
import java.util.Map;
import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.ledger.ReconciliationResult;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.tip.TipSettlement.Settled;
import com.doodlearena.proto.doodle.events.v1.CreditReason;
import com.doodlearena.proto.doodle.events.v1.WalletEvent;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * Tips out of escrow (ADR 0021): RoundFinished pays them to the round's drawers, the sweeper pays
 * late ones and refunds those whose round never finished. After every step each escrow shard holds
 * exactly its pending tips and the ledger balances.
 */
class TipSettlementTests extends ApiTestSupport {

	@Autowired
	private TipService tips;

	@Autowired
	private TipSettlement settlement;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private ReconciliationJob reconciliation;

	@Autowired
	private JdbcClient db;

	@Test
	void theRoundsDrawersShareItsTipsAndTheFirstGetsTheRemainder() {
		AccountRef first = this.fixtures.newUser();
		AccountRef second = this.fixtures.newUser();
		String match = match();
		for (int i = 0; i < 3; i++) {
			tip(this.fixtures.newUserWith(50), match, 1, 7);
		}

		Settled settled = this.settlement.roundFinished(round(match, 1, first, second));

		assertThat(settled).isEqualTo(new Settled(3, 0, 21));
		assertThat(this.fixtures.balance(first)).isEqualTo(11);
		assertThat(this.fixtures.balance(second)).isEqualTo(10);
		assertThat(statuses(match)).containsOnlyKeys("SETTLED");
		assertThat(credits(first.ownerId())).containsExactly(CreditReason.CREDIT_REASON_TIP);
		assertClean();
	}

	@Test
	void aDrawersOwnTipAndATipForAnotherRoomGoBack() {
		AccountRef drawer = this.fixtures.newUserWith(40);
		AccountRef viewer = this.fixtures.newUserWith(40);
		AccountRef lost = this.fixtures.newUserWith(40);
		String match = match();
		tip(drawer, match, 1, 10);
		tip(viewer, match, 1, 10);
		this.tips.tip(lost.ownerId(), "r_other", match, 1, 10);

		Settled settled = this.settlement.roundFinished(round(match, 1, drawer));

		assertThat(settled).isEqualTo(new Settled(1, 2, 10));
		// The drawer got the viewer's 10 and their own 10 back.
		assertThat(this.fixtures.balance(drawer)).isEqualTo(50);
		assertThat(this.fixtures.balance(viewer)).isEqualTo(30);
		assertThat(this.fixtures.balance(lost)).isEqualTo(40);
		assertThat(statuses(match)).isEqualTo(Map.of("SETTLED", 1L, "REFUNDED", 2L));
		assertThat(credits(lost.ownerId())).containsExactly(CreditReason.CREDIT_REASON_TIP_REFUND);
		assertClean();
	}

	@Test
	void aRedeliveredRoundPaysNothingMore() {
		AccountRef drawer = this.fixtures.newUser();
		String match = match();
		tip(this.fixtures.newUserWith(20), match, 2, 20);

		this.settlement.roundFinished(round(match, 2, drawer));
		Settled again = this.settlement.roundFinished(round(match, 2, drawer));

		assertThat(again).isEqualTo(new Settled(0, 0, 0));
		assertThat(this.fixtures.balance(drawer)).isEqualTo(20);
		assertThat(this.fixtures.transactionsWithKey("tip-settle:" + match + ":2:1")).isOne();
		assertThat(this.fixtures.transactionsWithKey("tip-settle:" + match + ":2:2")).isZero();
	}

	@Test
	void aTipThatLandsAfterItsRoundWasSettledIsPaidByTheSweeper() {
		AccountRef drawer = this.fixtures.newUser();
		String match = match();
		tip(this.fixtures.newUserWith(20), match, 1, 5);
		this.settlement.roundFinished(round(match, 1, drawer));

		tip(this.fixtures.newUserWith(20), match, 1, 6);
		this.settlement.sweep();

		assertThat(this.fixtures.balance(drawer)).isEqualTo(11);
		assertThat(statuses(match)).containsOnlyKeys("SETTLED");
		assertThat(this.fixtures.transactionsWithKey("tip-settle:" + match + ":1:2")).isOne();
		assertClean();
	}

	@Test
	void aTipWhoseRoundNeverFinishesGoesBackAfterTenMinutes() {
		AccountRef viewer = this.fixtures.newUserWith(30);
		String match = match();
		UUID fresh = tip(viewer, match, 1, 10).tipId();
		UUID stale = tip(viewer, match, 2, 10).tipId();
		this.db.sql("UPDATE tips SET created_at = now() - interval '11 minutes' WHERE tip_id = ?").param(stale).update();

		this.settlement.sweep();

		assertThat(this.fixtures.balance(viewer)).isEqualTo(20);
		assertThat(status(stale)).isEqualTo("REFUNDED");
		assertThat(status(fresh)).isEqualTo("PENDING");
		assertThat(this.fixtures.transactionsWithKey("tip-refund:" + stale)).isOne();
		assertClean();
	}

	@Test
	void aShardThatDoesntHoldItsPendingTipsIsReported() {
		Tip tip = tip(this.fixtures.newUserWith(10), match(), 1, 10);
		// Closed by hand without taking its coins out of escrow.
		this.db.sql("UPDATE tips SET status = 'SETTLED', closing_tx_id = tx_id, closed_at = now() WHERE tip_id = ?")
			.param(tip.tipId())
			.update();

		ReconciliationResult result = this.reconciliation.reconcile();
		assertThat(result.escrowMismatchedShards()).containsExactly(TipService.shardOf(tip.tipId()));
		assertThat(result.clean()).isFalse();

		// The tests share a database: put it back.
		this.db.sql("UPDATE tips SET status = 'PENDING', closing_tx_id = NULL, closed_at = NULL WHERE tip_id = ?")
			.param(tip.tipId())
			.update();
		assertClean();
	}

	@Test
	void twoDrawersSplitAnOddCoinAndAShareOfZeroIsLeftOut() {
		assertThat(TipSettlement.split(21, List.of("a", "b"))).containsExactly(Map.entry("a", 11L), Map.entry("b", 10L));
		assertThat(TipSettlement.split(1, List.of("a", "b"))).containsExactly(Map.entry("a", 1L));
		assertThat(TipSettlement.split(0, List.of("a"))).isEmpty();
	}

	/** Every ledger rule holds, and each shard holds exactly its pending tips. */
	private void assertClean() {
		ReconciliationResult result = this.reconciliation.reconcile();
		assertThat(result.escrowMismatchedShards()).isEmpty();
		assertThat(result.clean()).isTrue();
	}

	private Tip tip(AccountRef tipper, String match, int round, long amount) {
		return this.tips.tip(tipper.ownerId(), "r_1", match, round, amount);
	}

	private static RoundRequest round(String match, int round, AccountRef... drawers) {
		return new RoundRequest(match, "r_1", round, java.util.Arrays.stream(drawers).map(AccountRef::ownerId).toList());
	}

	private Map<String, Long> statuses(String match) {
		return this.db.sql("SELECT status, COUNT(*) FROM tips WHERE match_id = ? GROUP BY status")
			.param(match)
			.query((rs, i) -> Map.entry(rs.getString(1), rs.getLong(2)))
			.list()
			.stream()
			.collect(java.util.stream.Collectors.toMap(Map.Entry::getKey, Map.Entry::getValue));
	}

	private String status(UUID tip) {
		return this.db.sql("SELECT status FROM tips WHERE tip_id = ?").param(tip).query(String.class).single();
	}

	/** The reasons of the CoinsCredited events in the outbox for this user (funding a fixture posts none). */
	private List<CreditReason> credits(String userId) {
		return this.db.sql("SELECT payload FROM outbox WHERE topic = 'market.wallet.v1' AND key = ? ORDER BY created_at")
			.param(userId)
			.query((rs, i) -> {
				try {
					return WalletEvent.parseFrom(rs.getBytes(1)).getCoinsCredited().getReason();
				}
				catch (com.google.protobuf.InvalidProtocolBufferException ex) {
					throw new IllegalStateException(ex);
				}
			})
			.list();
	}

	private static String match() {
		return "m_" + UUID.randomUUID();
	}

}
