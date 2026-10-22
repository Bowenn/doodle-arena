package com.doodlearena.analytics;

import com.doodlearena.analytics.guesses.Guess;
import com.doodlearena.analytics.guesses.GuessDeserializer;
import com.doodlearena.analytics.guesses.TopGuessesQuery;
import com.doodlearena.analytics.guesses.TopGuessesSerializer;
import com.doodlearena.analytics.guesses.WindowTop;
import java.time.Duration;
import org.apache.flink.api.common.eventtime.WatermarkStrategy;
import org.apache.flink.connector.base.DeliveryGuarantee;
import org.apache.flink.connector.kafka.sink.KafkaSink;
import org.apache.flink.connector.kafka.source.KafkaSource;
import org.apache.flink.connector.kafka.source.enumerator.initializer.OffsetsInitializer;
import org.apache.flink.streaming.api.datastream.DataStream;
import org.apache.flink.streaming.api.environment.StreamExecutionEnvironment;
import org.apache.flink.table.api.bridge.java.StreamTableEnvironment;
import org.apache.kafka.clients.consumer.OffsetResetStrategy;

/**
 * The analytics service (ADR 0018): one Flink job, run in application mode on the flink image.
 * Its pipelines share the job's checkpoints (every 30 s, configured through FLINK_PROPERTIES).
 *
 * <ul>
 *   <li>Top guesses: live.guesses.v1 to live.top-guesses.v1 (TopGuessesQuery).
 * </ul>
 */
public final class AnalyticsJob {
	private AnalyticsJob() {}

	public static void main(String[] args) throws Exception {
		Config cfg = Config.fromEnv(System.getenv());
		StreamExecutionEnvironment env = StreamExecutionEnvironment.getExecutionEnvironment();
		build(env, cfg);
		env.execute("analytics");
	}

	/** Adds every pipeline to env; tests run the same graph against their own Kafka. */
	public static void build(StreamExecutionEnvironment env, Config cfg) {
		StreamTableEnvironment tEnv = StreamTableEnvironment.create(env);

		// Offsets live in Flink's checkpoints; the group's committed offsets are where a job
		// without a checkpoint starts, and what the lag metrics read. A new group starts at the
		// end: old guesses of rounds long over are of no use.
		KafkaSource<Guess> guessSource = KafkaSource.<Guess>builder()
				.setBootstrapServers(cfg.brokers())
				.setTopics(cfg.guessesTopic())
				.setGroupId(cfg.group())
				.setStartingOffsets(OffsetsInitializer.committedOffsets(
						cfg.fromEarliest() ? OffsetResetStrategy.EARLIEST : OffsetResetStrategy.LATEST))
				.setDeserializer(new GuessDeserializer())
				.setProperties(cfg.kafkaAuth())
				.build();
		// Event time is the relay's receive time; a guess may arrive this late and still count.
		// A partition without guesses (most rooms are quiet) must not hold the watermark back.
		DataStream<Guess> guesses = env.fromSource(guessSource,
				WatermarkStrategy.<Guess>forBoundedOutOfOrderness(cfg.outOfOrderness())
						.withTimestampAssigner((g, ts) -> g.guessedAtMs)
						.withIdleness(Duration.ofSeconds(10)),
				"live.guesses.v1").uid("guesses-source");

		DataStream<WindowTop> top = TopGuessesQuery.apply(tEnv, guesses, cfg.window(), cfg.slide(), cfg.topN());
		top.sinkTo(KafkaSink.<WindowTop>builder()
				.setBootstrapServers(cfg.brokers())
				.setKafkaProducerConfig(cfg.kafkaAuth())
				.setRecordSerializer(new TopGuessesSerializer(cfg.topGuessesTopic()))
				// At-least-once (ADR 0018): a TopGuesses replaces the room's last one, so a repeat
				// changes nothing, and exactly-once would hold every list until a checkpoint.
				.setDeliveryGuarantee(DeliveryGuarantee.AT_LEAST_ONCE)
				.build())
				.name("live.top-guesses.v1").uid("top-guesses-sink");
	}
}
