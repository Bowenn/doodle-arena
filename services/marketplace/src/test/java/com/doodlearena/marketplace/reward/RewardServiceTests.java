package com.doodlearena.marketplace.reward;

import java.util.ArrayList;
import java.util.List;
import java.util.Queue;
import java.util.UUID;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.reward.RewardService.RewardResult;
import com.doodlearena.marketplace.support.IntegrationTest;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.proto.doodle.events.v1.CoinsCredited;
import com.doodlearena.proto.doodle.events.v1.CreditReason;
import com.doodlearena.proto.doodle.events.v1.MatchFinished;
import com.doodlearena.proto.doodle.events.v1.PlayerResult;
import com.doodlearena.proto.doodle.events.v1.WalletEvent;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

@IntegrationTest
class RewardServiceTests {

	@Autowired
	private RewardService rewards;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private ReconciliationJob reconciliation;

	@Autowired
	private JdbcClient db;

	@Test
	void placementsAreRewardedAndTiesShareTheirPlacesAmount() {
		String match = "m_" + UUID.randomUUID();
		List<AccountRef> players = players(6);
		long issuanceBefore = this.fixtures.balance(AccountRef.REWARDS_ISSUANCE);
		// Scores 300, 200, 200, 100, 50, 0: competition ranking gives 1, 2, 2, 4, 5, 6.
		MatchFinished event = match(match, players, 1, 2, 2, 4, 5, 6);

		assertThat(this.rewards.pay(RewardRequest.from(event))).isEqualTo(RewardResult.PAID);

		assertThat(players).extracting(this.fixtures::balance).containsExactly(30L, 20L, 20L, 5L, 5L, 5L);
		// One ledger transaction: six credits and one debit of their sum.
		List<Long> entries = this.db.sql("""
				SELECT e.amount FROM ledger_entries e JOIN ledger_transactions t USING (tx_id)
				WHERE t.idempotency_key = ?""").param("reward:" + match).query(Long.class).list();
		assertThat(entries).hasSize(7).contains(-85L);
		assertThat(this.fixtures.balance(AccountRef.REWARDS_ISSUANCE)).isLessThanOrEqualTo(issuanceBefore - 85);
		assertThat(this.reconciliation.reconcile().clean()).isTrue();
	}

	@Test
	void eachPaidPlayerGetsCoinsCredited() throws Exception {
		String match = "m_" + UUID.randomUUID();
		List<AccountRef> players = players(3);

		this.rewards.pay(RewardRequest.from(match(match, players, 1, 2, 3)));

		List<CoinsCredited> events = new ArrayList<>();
		for (byte[] payload : this.db.sql("SELECT payload FROM outbox WHERE topic = 'market.wallet.v1' AND key IN (?, ?, ?)")
			.params(players.get(0).ownerId(), players.get(1).ownerId(), players.get(2).ownerId())
			.query(byte[].class)
			.list()) {
			events.add(WalletEvent.parseFrom(payload).getCoinsCredited());
		}
		assertThat(events).hasSize(3)
			.allSatisfy((e) -> assertThat(e.getReason()).isEqualTo(CreditReason.CREDIT_REASON_REWARD))
			.allSatisfy((e) -> assertThat(e.getReference()).isEqualTo(match))
			.extracting(CoinsCredited::getAmount)
			.containsExactlyInAnyOrder(30L, 20L, 10L);
	}

	/** M4: replaying a MatchFinished has no extra effect. */
	@Test
	void replayPaysNothingMore() {
		String match = "m_" + UUID.randomUUID();
		List<AccountRef> players = players(2);
		RewardRequest request = RewardRequest.from(match(match, players, 1, 2));

		assertThat(this.rewards.pay(request)).isEqualTo(RewardResult.PAID);
		assertThat(this.rewards.pay(request)).isEqualTo(RewardResult.DUPLICATE);

		assertThat(players).extracting(this.fixtures::balance).containsExactly(30L, 20L);
		assertThat(this.db.sql("SELECT COUNT(*) FROM outbox WHERE topic = 'market.wallet.v1' AND key = ?")
			.param(players.getFirst().ownerId())
			.query(Long.class)
			.single()).isEqualTo(1);
	}

	@Test
	void concurrentDeliveriesPayOnce() throws Exception {
		String match = "m_" + UUID.randomUUID();
		List<AccountRef> players = players(4);
		RewardRequest request = RewardRequest.from(match(match, players, 1, 2, 3, 4));
		Queue<RewardResult> results = new ConcurrentLinkedQueue<>();
		CountDownLatch start = new CountDownLatch(1);
		try (ExecutorService pool = Executors.newFixedThreadPool(10)) {
			List<Future<?>> futures = new ArrayList<>();
			for (int i = 0; i < 10; i++) {
				futures.add(pool.submit(() -> {
					start.await();
					results.add(this.rewards.pay(request));
					return null;
				}));
			}
			start.countDown();
			for (Future<?> future : futures) {
				future.get(1, TimeUnit.MINUTES);
			}
		}

		assertThat(results).hasSize(10).containsOnlyOnce(RewardResult.PAID);
		assertThat(players).extracting(this.fixtures::balance).containsExactly(30L, 20L, 10L, 5L);
	}

	@Test
	void soloMatchIsIneligible() {
		List<AccountRef> players = players(1);
		assertThat(this.rewards.pay(RewardRequest.from(match("m_" + UUID.randomUUID(), players, 1))))
			.isEqualTo(RewardResult.INELIGIBLE);
		assertThat(this.fixtures.balance(players.getFirst())).isZero();
	}

	@Test
	void eventsBreakingTheContractAreRefused() {
		List<AccountRef> players = players(2);
		MatchFinished valid = match("m_" + UUID.randomUUID(), players, 1, 2);
		assertThatThrownBy(() -> RewardRequest.from(valid.toBuilder().setMatchId(" ").build()))
			.isInstanceOf(InvalidMatchEventException.class);
		assertThatThrownBy(() -> RewardRequest.from(valid.toBuilder().addResults(valid.getResults(0)).build()))
			.isInstanceOf(InvalidMatchEventException.class);
		assertThatThrownBy(() -> RewardRequest.from(valid.toBuilder()
			.setResults(1, PlayerResult.newBuilder().setUserId("").setPlacement(2))
			.build())).isInstanceOf(InvalidMatchEventException.class);
		assertThatThrownBy(() -> RewardRequest.from(valid.toBuilder()
			.setResults(1, valid.getResults(1).toBuilder().setPlacement(0))
			.build())).isInstanceOf(InvalidMatchEventException.class);
		// More players than a room holds: not from a game server.
		List<AccountRef> crowd = players(RewardRequest.MAX_PLAYERS + 1);
		int[] placements = new int[crowd.size()];
		for (int i = 0; i < placements.length; i++) {
			placements[i] = i + 1;
		}
		assertThatThrownBy(() -> RewardRequest.from(match("m_" + UUID.randomUUID(), crowd, placements)))
			.isInstanceOf(InvalidMatchEventException.class);
		assertThat(RewardRequest.from(match("m_" + UUID.randomUUID(), crowd.subList(0, RewardRequest.MAX_PLAYERS),
				placements)).placements()).hasSize(RewardRequest.MAX_PLAYERS);
	}

	private List<AccountRef> players(int n) {
		List<AccountRef> players = new ArrayList<>();
		for (int i = 0; i < n; i++) {
			players.add(this.fixtures.newUser());
		}
		return players;
	}

	static MatchFinished match(String matchId, List<AccountRef> players, int... placements) {
		MatchFinished.Builder builder = MatchFinished.newBuilder().setMatchId(matchId).setRoomId("r_test");
		for (int i = 0; i < players.size(); i++) {
			builder.addResults(PlayerResult.newBuilder()
				.setUserId(players.get(i).ownerId())
				.setScore(100 * (players.size() - i))
				.setPlacement(placements[i]));
		}
		return builder.build();
	}

}
