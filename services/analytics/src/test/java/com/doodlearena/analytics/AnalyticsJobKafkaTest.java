package com.doodlearena.analytics;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;

import com.doodlearena.proto.doodle.events.v1.AnalyticsFlagEvent;
import com.doodlearena.proto.doodle.events.v1.SaleCompleted;
import com.doodlearena.proto.doodle.events.v1.SaleEvent;
import com.doodlearena.proto.doodle.events.v1.TopGuesses;
import com.doodlearena.proto.doodle.events.v1.TopGuessesEvent;
import com.doodlearena.proto.doodle.events.v1.ViewerGuess;
import com.doodlearena.proto.doodle.events.v1.ViewerGuessEvent;
import com.doodlearena.proto.doodle.events.v1.WashTradeFlagged;
import com.doodlearena.proto.doodle.events.v1.WashTradeRule;
import com.google.protobuf.Timestamp;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.stream.Collectors;
import org.apache.flink.core.execution.JobClient;
import org.apache.flink.runtime.testutils.MiniClusterResourceConfiguration;
import org.apache.flink.streaming.api.environment.StreamExecutionEnvironment;
import org.apache.flink.test.junit5.MiniClusterExtension;
import org.apache.kafka.clients.admin.Admin;
import org.apache.kafka.clients.admin.NewTopic;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.apache.kafka.clients.consumer.KafkaConsumer;
import org.apache.kafka.clients.producer.KafkaProducer;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.apache.kafka.common.serialization.ByteArrayDeserializer;
import org.apache.kafka.common.serialization.ByteArraySerializer;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.RegisterExtension;
import org.testcontainers.junit.jupiter.Container;
import org.testcontainers.junit.jupiter.Testcontainers;
import org.testcontainers.kafka.KafkaContainer;

/**
 * The whole job against a real broker (rule: .claude/rules/java.md): protobuf guesses in on
 * live.guesses.v1 and a TopGuesses out on live.top-guesses.v1; sales in on market.sales.v1 and a
 * WashTradeFlagged out on analytics.flags.v1.
 */
@Testcontainers
class AnalyticsJobKafkaTest {
	@Container
	static final KafkaContainer KAFKA = new KafkaContainer("apache/kafka:4.2.2");

	@RegisterExtension
	static final MiniClusterExtension MINI = new MiniClusterExtension(new MiniClusterResourceConfiguration.Builder()
			.setNumberTaskManagers(1).setNumberSlotsPerTaskManager(4).build());

	private static Properties client() {
		Properties p = new Properties();
		p.setProperty("bootstrap.servers", KAFKA.getBootstrapServers());
		return p;
	}

	private static ProducerRecord<byte[], byte[]> guess(String room, String text, long ms) {
		Timestamp at = Timestamp.newBuilder().setSeconds(ms / 1000).setNanos((int) (ms % 1000) * 1_000_000).build();
		byte[] value = ViewerGuessEvent.newBuilder().setViewerGuess(ViewerGuess.newBuilder()
				.setRoomId(room).setRoundNo(1).setText(text).setGuessedAt(at)).build().toByteArray();
		return new ProducerRecord<>("live.guesses.v1", room.getBytes(StandardCharsets.UTF_8), value);
	}

	private static ProducerRecord<byte[], byte[]> sale(String id, String drawing, String seller, String buyer, long price,
			long ms) {
		Timestamp at = Timestamp.newBuilder().setSeconds(ms / 1000).setNanos((int) (ms % 1000) * 1_000_000).build();
		byte[] value = SaleEvent.newBuilder().setSaleCompleted(SaleCompleted.newBuilder()
				.setSaleId(id).setDrawingId(drawing).setSellerId(seller).setBuyerId(buyer).setPrice(price)
				.setCompletedAt(at)).build().toByteArray();
		return new ProducerRecord<>("market.sales.v1", drawing.getBytes(StandardCharsets.UTF_8), value);
	}

	/** The first record on a topic within 60 s, or null. */
	private static ConsumerRecord<byte[], byte[]> first(String topic) {
		Properties consumer = client();
		consumer.setProperty("group.id", "test-" + topic);
		consumer.setProperty("auto.offset.reset", "earliest");
		consumer.setProperty("key.deserializer", ByteArrayDeserializer.class.getName());
		consumer.setProperty("value.deserializer", ByteArrayDeserializer.class.getName());
		try (KafkaConsumer<byte[], byte[]> c = new KafkaConsumer<>(consumer)) {
			c.subscribe(List.of(topic));
			long deadline = System.currentTimeMillis() + 60_000;
			while (System.currentTimeMillis() < deadline) {
				for (ConsumerRecord<byte[], byte[]> r : c.poll(Duration.ofMillis(500))) {
					return r;
				}
			}
		}
		return null;
	}

	@Test
	void guessesAndSalesInTopGuessesAndFlagsOut() throws Exception {
		try (Admin admin = Admin.create(client())) {
			admin.createTopics(List.of(new NewTopic("live.guesses.v1", 1, (short) 1),
					new NewTopic("live.top-guesses.v1", 1, (short) 1),
					new NewTopic("market.sales.v1", 1, (short) 1),
					new NewTopic("analytics.flags.v1", 1, (short) 1))).all().get();
		}
		long t0 = 1_800_000_000_000L; // a multiple of 10 s
		Properties producer = client();
		producer.setProperty("key.serializer", ByteArraySerializer.class.getName());
		producer.setProperty("value.serializer", ByteArraySerializer.class.getName());
		try (KafkaProducer<byte[], byte[]> p = new KafkaProducer<>(producer)) {
			for (String text : List.of("dog", "cat", "dog", "sun", "dog", "cat")) {
				p.send(guess("room1", text, t0 + 1_000));
			}
			// Much later: moves the watermark past the first window, which then closes.
			p.send(guess("room1", "zzz", t0 + 60_000));
			// d1 goes from alice to bob and back within an hour: a round trip.
			p.send(sale("s1", "d1", "alice", "bob", 100, t0));
			p.send(sale("s2", "d1", "bob", "alice", 150, t0 + 3_600_000));
			p.flush();
		}

		Config cfg = Config.fromEnv(Map.of(
				"KAFKA_BROKERS", KAFKA.getBootstrapServers(), "GUESSES_START", "earliest",
				"TOP_GUESSES_WINDOW_SECONDS", "10", "TOP_GUESSES_SLIDE_SECONDS", "10", "TOP_GUESSES_N", "2"));
		StreamExecutionEnvironment env = StreamExecutionEnvironment.getExecutionEnvironment();
		env.setParallelism(1);
		AnalyticsJob.build(env, cfg);
		JobClient job = env.executeAsync("analytics-test");
		try {
			ConsumerRecord<byte[], byte[]> r = first("live.top-guesses.v1");
			assertNotNull(r, "no TopGuesses within 60 s");
			assertEquals("room1", new String(r.key(), StandardCharsets.UTF_8), "keyed by room");
			TopGuesses got = TopGuessesEvent.parseFrom(r.value()).getTopGuesses();
			assertEquals("room1", got.getRoomId());
			assertEquals((t0 + 10_000) / 1000, got.getWindowEnd().getSeconds());
			assertEquals("dog=3 cat=2", got.getGuessesList().stream()
					.map(g -> g.getText() + "=" + g.getCount()).collect(Collectors.joining(" ")));
			assertEquals("analytics", got.getMeta().getProducer());

			r = first("analytics.flags.v1");
			assertNotNull(r, "no WashTradeFlagged within 60 s");
			assertEquals("d1", new String(r.key(), StandardCharsets.UTF_8), "keyed by drawing");
			WashTradeFlagged flag = AnalyticsFlagEvent.parseFrom(r.value()).getWashTradeFlagged();
			assertEquals(WashTradeRule.WASH_TRADE_RULE_ROUND_TRIP, flag.getRule());
			assertEquals(List.of("alice", "bob", "alice"), flag.getAccountIdsList());
			assertEquals(List.of("s1", "s2"), flag.getSaleIdsList());
			assertEquals(250, flag.getVolume());
			assertEquals(flag.getFlagId(), flag.getMeta().getEventId());
		} finally {
			job.cancel().get();
		}
	}
}
