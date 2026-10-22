package com.doodlearena.analytics;

import java.time.Duration;
import java.util.Map;
import java.util.Properties;

/**
 * The job's settings, from the environment (ADR 0018). Flink's own settings (checkpoints, memory,
 * the metric reporter) come from FLINK_PROPERTIES instead, which the image writes into Flink's
 * configuration.
 */
public record Config(
		String brokers, String username, String password, String caPem, String group,
		String guessesTopic, String topGuessesTopic, Duration window, Duration slide, int topN,
		Duration outOfOrderness, boolean fromEarliest, String salesTopic, String flagsTopic, Duration washWindow) {

	/** Reads the environment, with the local stack's defaults. */
	public static Config fromEnv(Map<String, String> env) {
		return new Config(
				env.getOrDefault("KAFKA_BROKERS", "localhost:9094"),
				env.getOrDefault("KAFKA_USERNAME", ""),
				env.getOrDefault("KAFKA_PASSWORD", ""),
				env.getOrDefault("KAFKA_CA_PEM", ""),
				env.getOrDefault("KAFKA_GROUP", "analytics"),
				env.getOrDefault("GUESSES_TOPIC", "live.guesses.v1"),
				env.getOrDefault("TOP_GUESSES_TOPIC", "live.top-guesses.v1"),
				Duration.ofSeconds(Long.parseLong(env.getOrDefault("TOP_GUESSES_WINDOW_SECONDS", "30"))),
				Duration.ofSeconds(Long.parseLong(env.getOrDefault("TOP_GUESSES_SLIDE_SECONDS", "2"))),
				Integer.parseInt(env.getOrDefault("TOP_GUESSES_N", "5")),
				Duration.ofSeconds(Long.parseLong(env.getOrDefault("GUESSES_OUT_OF_ORDERNESS_SECONDS", "2"))),
				// Where a job without a checkpoint or committed offsets starts: the end, unless a
				// test asks for the whole topic.
				"earliest".equals(env.get("GUESSES_START")),
				env.getOrDefault("SALES_TOPIC", "market.sales.v1"),
				env.getOrDefault("FLAGS_TOPIC", "analytics.flags.v1"),
				// How far back a loop of sales counts as wash trading (ADR 0018: 7 days).
				Duration.ofHours(Long.parseLong(env.getOrDefault("WASH_TRADE_WINDOW_HOURS", "168"))));
	}

	/**
	 * The Kafka client's authentication, as every service has it (ADR 0013, libs/go/kafkaauth):
	 * SCRAM-SHA-512 when KAFKA_USERNAME is set, over TLS with the stack's CA when KAFKA_CA_PEM is
	 * set (AWS), and nothing in tests.
	 */
	public Properties kafkaAuth() {
		Properties p = new Properties();
		if (username.isEmpty()) {
			if (!caPem.isEmpty()) {
				throw new IllegalArgumentException("kafka: KAFKA_CA_PEM is set without KAFKA_USERNAME");
			}
			return p;
		}
		if (password.isEmpty()) {
			throw new IllegalArgumentException("kafka: KAFKA_USERNAME is set without KAFKA_PASSWORD");
		}
		p.setProperty("security.protocol", caPem.isEmpty() ? "SASL_PLAINTEXT" : "SASL_SSL");
		p.setProperty("sasl.mechanism", "SCRAM-SHA-512");
		p.setProperty("sasl.jaas.config", "org.apache.kafka.common.security.scram.ScramLoginModule required "
				+ "username=\"" + username + "\" password=\"" + password + "\";");
		if (!caPem.isEmpty()) {
			p.setProperty("ssl.truststore.type", "PEM");
			p.setProperty("ssl.truststore.certificates", caPem);
		}
		return p;
	}
}
