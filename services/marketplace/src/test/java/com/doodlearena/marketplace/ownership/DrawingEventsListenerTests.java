package com.doodlearena.marketplace.ownership;

import java.time.Duration;
import java.util.UUID;

import org.junit.jupiter.api.Test;
import org.testcontainers.postgresql.PostgreSQLContainer;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.kafka.core.KafkaTemplate;

import com.doodlearena.marketplace.support.KafkaListenerTestSupport;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.proto.doodle.events.v1.DrawingApproved;
import com.doodlearena.proto.doodle.events.v1.DrawingEvent;
import com.doodlearena.proto.doodle.events.v1.DrawingRejected;

import static org.assertj.core.api.Assertions.assertThat;
import static org.awaitility.Awaitility.await;

/**
 * The consumer against a real broker and database. One partition, so records are handled in the
 * order they were sent: when a later record is done, every earlier one was handled too.
 */
class DrawingEventsListenerTests extends KafkaListenerTestSupport {

	private static final Duration WAIT = Duration.ofSeconds(60);

	@Autowired
	private KafkaTemplate<String, byte[]> kafka;

	@Autowired
	private JdbcClient db;

	@Autowired
	private PostgreSQLContainer postgres;

	@Test
	void approvedDrawingIsMintedOnceThoughDeliveredTwice() {
		UUID drawing = UUID.randomUUID();
		byte[] event = approvedEvent(drawing, "u_ada", "u_bob");

		send(drawing, event);
		send(drawing, event);
		UUID marker = sendMarker();

		await().atMost(WAIT).until(() -> minted(marker));
		assertThat(owner(drawing)).isEqualTo("u_ada");
		assertThat(mintEvents(drawing)).isEqualTo(1);
	}

	@Test
	void rejectedDrawingIsNotMinted() {
		UUID drawing = UUID.randomUUID();
		send(drawing, DrawingEvent.newBuilder()
			.setDrawingRejected(DrawingRejected.newBuilder().setDrawingId(drawing.toString()).addAuthorIds("u_cyd"))
			.build()
			.toByteArray());
		UUID marker = sendMarker();

		await().atMost(WAIT).until(() -> minted(marker));
		assertThat(owner(drawing)).isNull();
	}

	@Test
	void unreadableAndInvalidRecordsDoNotBlockThePartition() {
		UUID invalid = UUID.randomUUID();
		send(UUID.randomUUID(), new byte[] { 0x0a, (byte) 0xff, 0x01 }); // truncated protobuf
		send(UUID.randomUUID(), DrawingEvent.getDefaultInstance().toByteArray()); // no event set
		send(UUID.randomUUID(), new byte[] { 0x7a, 0x01, 0x00 }); // field 15: an event from a newer producer
		// Readable but breaks the contract: the owner isn't the first author.
		send(invalid, DrawingEvent.newBuilder()
			.setDrawingApproved(DrawingApproved.newBuilder()
				.setDrawingId(invalid.toString())
				.addAuthorIds("u_dee")
				.setInitialOwnerId("u_mallory"))
			.build()
			.toByteArray());
		UUID marker = sendMarker();

		await().atMost(WAIT).until(() -> minted(marker));
		assertThat(owner(invalid)).isNull();
	}

	@Test
	void databaseOutageIsRetriedNotSkipped() throws Exception {
		UUID drawing = UUID.randomUUID();
		var docker = this.postgres.getDockerClient();
		docker.pauseContainerCmd(this.postgres.getContainerId()).exec();
		try {
			send(drawing, approvedEvent(drawing, "u_eve"));
			// Long enough for several failed attempts (connection and socket timeouts of 2-3 s).
			Thread.sleep(10_000);
		}
		finally {
			docker.unpauseContainerCmd(this.postgres.getContainerId()).exec();
		}

		await().atMost(WAIT).until(() -> owner(drawing) != null);
		assertThat(owner(drawing)).isEqualTo("u_eve");
		assertThat(mintEvents(drawing)).isEqualTo(1);
	}

	/** A valid drawing sent last: once it is minted, everything sent before it was handled. */
	private UUID sendMarker() {
		UUID marker = UUID.randomUUID();
		send(marker, approvedEvent(marker, "u_marker"));
		return marker;
	}

	private void send(UUID key, byte[] value) {
		this.kafka.send(DrawingEventsListener.TOPIC, key.toString(), value).join();
	}

	private boolean minted(UUID drawing) {
		return owner(drawing) != null;
	}

	private String owner(UUID drawing) {
		return this.db.sql("SELECT owner_id FROM ownership WHERE drawing_id = ?")
			.param(drawing)
			.query(String.class)
			.optional()
			.orElse(null);
	}

	private long mintEvents(UUID drawing) {
		return this.db.sql("SELECT COUNT(*) FROM ownership_events WHERE drawing_id = ? AND kind = 'MINT'")
			.param(drawing)
			.query(Long.class)
			.single();
	}

	private static byte[] approvedEvent(UUID drawing, String... authors) {
		return DrawingEvent.newBuilder()
			.setDrawingApproved(TestEvents.approved(drawing, authors))
			.build()
			.toByteArray();
	}

}
