package com.doodlearena.marketplace.reward;

import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.kafka.core.KafkaTemplate;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.support.KafkaListenerTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.proto.doodle.events.v1.MatchEvent;
import com.doodlearena.proto.doodle.events.v1.MatchFinished;
import com.doodlearena.proto.doodle.events.v1.PlayerResult;

import static org.assertj.core.api.Assertions.assertThat;
import static org.awaitility.Awaitility.await;

/**
 * The rewards consumer against a real broker. One partition, so records are handled in the order
 * they were sent: once the last one is paid, every earlier one was handled.
 */
class MatchEventsListenerTests extends KafkaListenerTestSupport {

	private static final Duration WAIT = Duration.ofSeconds(60);

	@Autowired
	private KafkaTemplate<String, byte[]> kafka;

	@Autowired
	private LedgerFixtures fixtures;

	/** M4: replaying a MatchFinished has no extra effect. */
	@Test
	void finishedMatchIsPaidOnceThoughDeliveredTwice() {
		List<AccountRef> players = players(3);
		byte[] event = finished("m_" + UUID.randomUUID(), players);

		send(event);
		send(event);
		AccountRef marker = sendMarker();

		await().atMost(WAIT).until(() -> this.fixtures.balance(marker) > 0);
		assertThat(players).extracting(this.fixtures::balance).containsExactly(30L, 20L, 10L);
	}

	@Test
	void unreadableAndInvalidRecordsDoNotBlockThePartition() {
		List<AccountRef> invalid = players(2);
		send(new byte[] { 0x0a, (byte) 0xff, 0x01 });
		send(MatchEvent.getDefaultInstance().toByteArray());
		// A repeated player breaks the contract, so nobody in it is paid.
		send(MatchEvent.newBuilder()
			.setMatchFinished(MatchFinished.newBuilder()
				.setMatchId("m_" + UUID.randomUUID())
				.addResults(PlayerResult.newBuilder().setUserId(invalid.get(0).ownerId()).setPlacement(1))
				.addResults(PlayerResult.newBuilder().setUserId(invalid.get(0).ownerId()).setPlacement(2)))
			.build()
			.toByteArray());
		AccountRef marker = sendMarker();

		await().atMost(WAIT).until(() -> this.fixtures.balance(marker) > 0);
		assertThat(this.fixtures.balance(invalid.get(0))).isZero();
	}

	/** A two-player match sent last: once its winner is paid, everything before it was handled. */
	private AccountRef sendMarker() {
		List<AccountRef> players = players(2);
		send(finished("m_" + UUID.randomUUID(), players));
		return players.getFirst();
	}

	private void send(byte[] value) {
		this.kafka.send(MatchEventsListener.TOPIC, "r_test", value).join();
	}

	private List<AccountRef> players(int n) {
		List<AccountRef> players = new ArrayList<>();
		for (int i = 0; i < n; i++) {
			players.add(this.fixtures.newUser());
		}
		return players;
	}

	private static byte[] finished(String matchId, List<AccountRef> players) {
		int[] placements = new int[players.size()];
		for (int i = 0; i < placements.length; i++) {
			placements[i] = i + 1;
		}
		return MatchEvent.newBuilder()
			.setMatchFinished(RewardServiceTests.match(matchId, players, placements))
			.build()
			.toByteArray();
	}

}
