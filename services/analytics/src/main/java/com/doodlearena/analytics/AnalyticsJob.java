package com.doodlearena.analytics;

import com.doodlearena.analytics.guesses.Guess;
import com.doodlearena.analytics.guesses.GuessDeserializer;
import com.doodlearena.analytics.guesses.TopGuessesQuery;
import com.doodlearena.analytics.guesses.TopGuessesSerializer;
import com.doodlearena.analytics.guesses.WindowTop;
import com.doodlearena.analytics.wash.Flag;
import com.doodlearena.analytics.wash.FlagSerializer;
import com.doodlearena.analytics.wash.Sale;
import com.doodlearena.analytics.wash.SaleDeserializer;
import com.doodlearena.analytics.wash.WashTradeDetector;
import java.time.Duration;
import java.util.Optional;
import org.apache.flink.configuration.CheckpointingOptions;
import org.apache.flink.configuration.Configuration;
import org.apache.flink.configuration.ExternalizedCheckpointRetention;
import org.apache.flink.configuration.GlobalConfiguration;
import org.apache.flink.configuration.StateRecoveryOptions;
import org.apache.flink.core.execution.RecoveryClaimMode;
import org.apache.flink.api.common.eventtime.WatermarkStrategy;
import org.apache.flink.connector.base.DeliveryGuarantee;
import org.apache.flink.connector.kafka.sink.KafkaSink;
import org.apache.flink.connector.kafka.source.KafkaSource;
import org.apache.flink.connector.kafka.source.enumerator.initializer.OffsetsInitializer;
import org.apache.flink.streaming.api.datastream.DataStream;
import org.apache.flink.streaming.api.environment.StreamExecutionEnvironment;
import org.apache.flink.table.api.bridge.java.StreamTableEnvironment;
import org.apache.kafka.clients.consumer.OffsetResetStrategy;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * The analytics service (ADR 0018): one Flink job, run in application mode on the flink image.
 * Its pipelines share the job's checkpoints (every 30 s, configured through FLINK_PROPERTIES).
 *
 * <ul>
 *   <li>Top guesses: live.guesses.v1 to live.top-guesses.v1 (TopGuessesQuery).
 *   <li>Wash trading: market.sales.v1 to analytics.flags.v1 (WashTradeDetector).
 * </ul>
 */
public final class AnalyticsJob {
	private static final Logger LOG = LoggerFactory.getLogger(AnalyticsJob.class);

	private AnalyticsJob() {}

	public static void main(String[] args) throws Exception {
		Config cfg = Config.fromEnv(System.getenv());
		StreamExecutionEnvironment env = StreamExecutionEnvironment.getExecutionEnvironment(startConfig());
		build(env, cfg);
		env.execute("analytics");
	}

	/**
	 * How the job starts and keeps its checkpoints (ADR 0018). main runs inside the JobManager
	 * (application mode), whose configuration (FLINK_PROPERTIES included) names the checkpoint
	 * directory.
	 */
	private static Configuration startConfig() throws Exception {
		Configuration conf = new Configuration();
		// Kept when the JobManager stops, so the next one can start from it.
		conf.set(CheckpointingOptions.EXTERNALIZED_CHECKPOINT_RETENTION, ExternalizedCheckpointRetention.RETAIN_ON_CANCELLATION);
		String dir = GlobalConfiguration.loadConfiguration().get(CheckpointingOptions.CHECKPOINTS_DIRECTORY);
		Optional<String> from = CheckpointRestore.choose(System.getenv("ANALYTICS_RESTORE"), dir);
		if (from.isPresent()) {
			LOG.info("starting from checkpoint {}", from.get());
			conf.set(StateRecoveryOptions.SAVEPOINT_PATH, from.get());
			// The job owns the checkpoint it starts from and deletes it once a newer one exists, so
			// the bucket keeps one checkpoint, whatever job id made it.
			conf.set(StateRecoveryOptions.RESTORE_MODE, RecoveryClaimMode.CLAIM);
			// Flink SQL names its operators itself, and any change to the job's graph renames them,
			// so their state (top guesses' open windows, at most 30 s) can't be mapped after a
			// deploy and would stop the job from starting. It is dropped instead. The state that
			// matters has fixed uids and is restored: the sources' offsets and wash trading's
			// 7 days of sales. Never change those uids (guesses-source, sales-source, wash-trades).
			conf.set(StateRecoveryOptions.SAVEPOINT_IGNORE_UNCLAIMED_STATE, true);
		} else {
			LOG.info("starting without state (no completed checkpoint in {}, ANALYTICS_RESTORE={})", dir,
					System.getenv("ANALYTICS_RESTORE"));
		}
		return conf;
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

		// A new group reads every sale still on the topic: the history is what a loop is made of.
		KafkaSource<Sale> saleSource = KafkaSource.<Sale>builder()
				.setBootstrapServers(cfg.brokers())
				.setTopics(cfg.salesTopic())
				.setGroupId(cfg.group())
				.setStartingOffsets(OffsetsInitializer.committedOffsets(OffsetResetStrategy.EARLIEST))
				.setDeserializer(new SaleDeserializer())
				.setProperties(cfg.kafkaAuth())
				.build();
		// Event time is the sale's commit time. Watermarks only clear old sales from state (the
		// detector compares the sales' own times), and sales are rare, so partitions go idle.
		DataStream<Sale> sales = env.fromSource(saleSource,
				WatermarkStrategy.<Sale>forBoundedOutOfOrderness(Duration.ofSeconds(5))
						.withTimestampAssigner((s, ts) -> s.completedAtMs)
						.withIdleness(Duration.ofMinutes(1)),
				"market.sales.v1").uid("sales-source");
		DataStream<Flag> flags = sales.keyBy(s -> s.drawingId)
				.process(new WashTradeDetector(cfg.washWindow()))
				.name("wash trading").uid("wash-trades");
		flags.sinkTo(KafkaSink.<Flag>builder()
				.setBootstrapServers(cfg.brokers())
				.setKafkaProducerConfig(cfg.kafkaAuth())
				.setRecordSerializer(new FlagSerializer(cfg.flagsTopic()))
				// At-least-once (ADR 0018): a repeated flag carries the same flag_id.
				.setDeliveryGuarantee(DeliveryGuarantee.AT_LEAST_ONCE)
				.build())
				.name("analytics.flags.v1").uid("flags-sink");
	}
}
