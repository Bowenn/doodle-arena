package com.doodlearena.marketplace.ownership;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.dao.DataIntegrityViolationException;
import org.springframework.kafka.listener.DefaultErrorHandler;
import org.springframework.util.backoff.ExponentialBackOff;

import com.doodlearena.marketplace.reward.MatchEventsListener;
import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * What happens when a listener throws. The default is to retry the same record, waiting 1 s
 * doubling to 30 s, with no limit: a mint is never skipped because Postgres was down for a while.
 * The partition waits instead, which shows as consumer lag. A code bug is retried the same way,
 * on purpose: fix it and redeploy, and nothing was lost.
 *
 * <p>The exception is a record that would fail forever: one Postgres refuses as data (a value out
 * of range), or one Spring Kafka can't hand to the listener. It is logged, counted as failed and
 * committed.
 */
@Configuration(proxyBeanMethods = false)
public class KafkaConfig {

	private static final Logger log = LoggerFactory.getLogger(KafkaConfig.class);

	@Bean
	DefaultErrorHandler kafkaErrorHandler(MarketMetrics metrics) {
		ExponentialBackOff backOff = new ExponentialBackOff(1_000, 2.0);
		backOff.setMaxInterval(30_000);
		DefaultErrorHandler handler = new DefaultErrorHandler((record, ex) -> {
			log.atError()
				.addKeyValue("topic", record.topic())
				.addKeyValue("partition", record.partition())
				.addKeyValue("offset", record.offset())
				.setCause(ex)
				.log("record refused by the database; skipped");
			if (record.topic().equals(MatchEventsListener.TOPIC)) {
				metrics.matchReceived("failed");
			}
			else {
				metrics.drawingReceived("failed");
			}
		}, backOff);
		// Classified by the first cause that is listed: Spring Kafka's own list (records it can't
		// convert to the listener's arguments) plus this one; everything else is retried.
		handler.addNotRetryableExceptions(DataIntegrityViolationException.class);
		return handler;
	}

}
