package com.doodlearena.marketplace.tip;

import java.time.Duration;
import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.kafka.core.KafkaTemplate;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.support.KafkaListenerTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.proto.doodle.events.v1.RoundEvent;
import com.doodlearena.proto.doodle.events.v1.RoundFinished;

import static org.assertj.core.api.Assertions.assertThat;
import static org.awaitility.Awaitility.await;

/**
 * The round consumer against a real broker (ADR 0021). One partition, so records are handled in
 * the order they were sent: once the last round is paid, every earlier record was handled.
 */
class RoundEventsListenerTests extends KafkaListenerTestSupport {

	private static final Duration WAIT = Duration.ofSeconds(60);

	@Autowired
	private KafkaTemplate<String, byte[]> kafka;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private TipService tips;

	@Test
	void aFinishedRoundPaysItsTipsOnceThoughDeliveredTwice() {
		AccountRef drawer = this.fixtures.newUser();
		String match = "m_" + UUID.randomUUID();
		this.tips.tip(this.fixtures.newUserWith(30).ownerId(), "r_test", match, 1, 12);
		byte[] round = finished(match, 1, drawer.ownerId());

		send(round);
		send(round);
		AccountRef marker = sendMarker();

		await().atMost(WAIT).until(() -> this.fixtures.balance(marker) > 0);
		assertThat(this.fixtures.balance(drawer)).isEqualTo(12);
	}

	@Test
	void unreadableAndInvalidRecordsDoNotBlockThePartition() {
		AccountRef drawer = this.fixtures.newUser();
		String match = "m_" + UUID.randomUUID();
		this.tips.tip(this.fixtures.newUserWith(30).ownerId(), "r_test", match, 1, 12);
		send(new byte[] { 0x0a, (byte) 0xff, 0x01 });
		send(RoundEvent.getDefaultInstance().toByteArray());
		// The same drawer twice breaks the contract, so the round isn't settled.
		send(finished(match, 1, drawer.ownerId(), drawer.ownerId()));
		AccountRef marker = sendMarker();

		await().atMost(WAIT).until(() -> this.fixtures.balance(marker) > 0);
		assertThat(this.fixtures.balance(drawer)).isZero();
	}

	/** A tipped round sent last: once its drawer is paid, everything before it was handled. */
	private AccountRef sendMarker() {
		AccountRef drawer = this.fixtures.newUser();
		String match = "m_" + UUID.randomUUID();
		this.tips.tip(this.fixtures.newUserWith(1).ownerId(), "r_test", match, 1, 1);
		send(finished(match, 1, drawer.ownerId()));
		return drawer;
	}

	private void send(byte[] value) {
		this.kafka.send(RoundEventsListener.TOPIC, "r_test", value).join();
	}

	private static byte[] finished(String match, int roundNo, String... drawers) {
		return RoundEvent.newBuilder()
			.setRoundFinished(RoundFinished.newBuilder()
				.setMatchId(match)
				.setRoomId("r_test")
				.setRoundNo(roundNo)
				.addAllDrawerIds(java.util.List.of(drawers)))
			.build()
			.toByteArray();
	}

}
