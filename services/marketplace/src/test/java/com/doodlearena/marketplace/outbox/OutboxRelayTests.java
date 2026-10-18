package com.doodlearena.marketplace.outbox;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import com.google.protobuf.InvalidProtocolBufferException;
import org.apache.kafka.clients.consumer.ConsumerConfig;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.apache.kafka.clients.consumer.KafkaConsumer;
import org.apache.kafka.common.serialization.ByteArrayDeserializer;
import org.apache.kafka.common.serialization.StringDeserializer;
import org.junit.jupiter.api.Test;
import org.testcontainers.kafka.KafkaContainer;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.context.annotation.Import;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.annotation.DirtiesContext;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.support.KafkaTestConfiguration;
import com.doodlearena.marketplace.support.PostgresTestConfiguration;
import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.SaleCompleted;
import com.doodlearena.proto.doodle.events.v1.SaleEvent;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * The relay against a real broker. Its scheduled pass stays off here (the test properties), so
 * each test runs {@link OutboxRelay#publishBatch} itself and knows exactly when it happened.
 */
@SpringBootTest
@Import({ PostgresTestConfiguration.class, KafkaTestConfiguration.class })
@DirtiesContext
class OutboxRelayTests {

	@Autowired
	private OutboxRelay relay;

	@Autowired
	private OutboxWriter outbox;

	@Autowired
	private TransactionTemplate tx;

	@Autowired
	private JdbcClient db;

	@Autowired
	private KafkaContainer kafka;

	@Test
	void publishesAndMarksTheBatch() {
		String drawing = UUID.randomUUID().toString();
		EventMeta first = write(drawing, 10);
		EventMeta second = write(drawing, 20);

		assertThat(this.relay.publishBatch()).isEqualTo(2);

		assertThat(unpublished(first)).isFalse();
		assertThat(unpublished(second)).isFalse();
		List<ConsumerRecord<String, byte[]>> records = readAll(OutboxWriter.SALES_TOPIC, drawing, 2);
		assertThat(records).extracting(ConsumerRecord::key).containsOnly(drawing);
		assertThat(records).extracting((r) -> parse(r).getSaleCompleted().getMeta().getEventId())
			.containsExactly(first.getEventId(), second.getEventId());
		assertThat(this.relay.publishBatch()).isZero();
	}

	@Test
	void kafkaOutageLeavesRowsForTheNextPass() {
		String drawing = UUID.randomUUID().toString();
		EventMeta meta = write(drawing, 30);
		var docker = this.kafka.getDockerClient();
		docker.pauseContainerCmd(this.kafka.getContainerId()).exec();
		int published;
		try {
			published = this.relay.publishBatch();
		}
		finally {
			docker.unpauseContainerCmd(this.kafka.getContainerId()).exec();
		}
		assertThat(published).isZero();
		assertThat(unpublished(meta)).isTrue();

		assertThat(this.relay.publishBatch()).isEqualTo(1);
		assertThat(unpublished(meta)).isFalse();
		// At least once: the producer may still deliver the first, timed-out send after the broker
		// comes back, so the event can arrive twice. Every copy has the outbox's event id, which is
		// what consumers dedupe on.
		List<ConsumerRecord<String, byte[]>> records = readAll(OutboxWriter.SALES_TOPIC, drawing, 1);
		assertThat(records).isNotEmpty()
			.allSatisfy((r) -> assertThat(parse(r).getSaleCompleted().getMeta().getEventId()).isEqualTo(meta.getEventId()));
	}

	@Test
	void eventsOfARolledBackTransactionNeverExist() {
		String drawing = UUID.randomUUID().toString();
		this.tx.executeWithoutResult((status) -> {
			write(drawing, 40);
			status.setRollbackOnly();
		});
		assertThat(this.db.sql("SELECT COUNT(*) FROM outbox WHERE key = ?").param(drawing).query(Long.class).single()).isZero();
	}

	@Test
	void publishedEventsAreDeletedAfterTheRetention() {
		String drawing = UUID.randomUUID().toString();
		EventMeta old = write(drawing, 1);
		EventMeta recent = write(drawing, 2);
		EventMeta waiting = write(drawing, 3);
		ageRow(old, "published_at = now() - interval '8 days', created_at = now() - interval '8 days'");
		ageRow(recent, "published_at = now() - interval '6 days'");
		// Never published, however old: deleting it would lose the event.
		ageRow(waiting, "created_at = now() - interval '30 days'");

		assertThat(this.relay.deletePublished()).isGreaterThanOrEqualTo(1);
		assertThat(this.db.sql("SELECT event_id FROM outbox WHERE key = ?").param(drawing).query(UUID.class).list())
			.containsExactlyInAnyOrder(UUID.fromString(recent.getEventId()), UUID.fromString(waiting.getEventId()));
	}

	private void ageRow(EventMeta meta, String set) {
		this.db.sql("UPDATE outbox SET " + set + " WHERE event_id = ?").param(UUID.fromString(meta.getEventId())).update();
	}

	private EventMeta write(String drawing, long price) {
		EventMeta meta = OutboxWriter.meta("SaleCompleted", Instant.now());
		byte[] payload = SaleEvent.newBuilder()
			.setSaleCompleted(SaleCompleted.newBuilder().setMeta(meta).setDrawingId(drawing).setPrice(price))
			.build()
			.toByteArray();
		this.tx.executeWithoutResult((status) -> this.outbox.write(OutboxWriter.SALES_TOPIC, drawing, meta, payload));
		return meta;
	}

	private boolean unpublished(EventMeta meta) {
		return this.db.sql("SELECT published_at IS NULL FROM outbox WHERE event_id = ?")
			.param(UUID.fromString(meta.getEventId()))
			.query(Boolean.class)
			.single();
	}

	/** The records with this key, read from the start of the topic. */
	private List<ConsumerRecord<String, byte[]>> readAll(String topic, String key, int expected) {
		List<ConsumerRecord<String, byte[]>> found = new ArrayList<>();
		try (KafkaConsumer<String, byte[]> consumer = new KafkaConsumer<>(Map.of(
				ConsumerConfig.BOOTSTRAP_SERVERS_CONFIG, this.kafka.getBootstrapServers(),
				ConsumerConfig.GROUP_ID_CONFIG, "test-" + UUID.randomUUID(),
				ConsumerConfig.AUTO_OFFSET_RESET_CONFIG, "earliest"), new StringDeserializer(), new ByteArrayDeserializer())) {
			consumer.subscribe(List.of(topic));
			long deadline = System.nanoTime() + Duration.ofSeconds(30).toNanos();
			while (found.size() < expected && System.nanoTime() < deadline) {
				for (ConsumerRecord<String, byte[]> record : consumer.poll(Duration.ofMillis(500))) {
					if (key.equals(record.key())) {
						found.add(record);
					}
				}
			}
		}
		return found;
	}

	private static SaleEvent parse(ConsumerRecord<String, byte[]> record) {
		try {
			return SaleEvent.parseFrom(record.value());
		}
		catch (InvalidProtocolBufferException ex) {
			throw new AssertionError(ex);
		}
	}

}
