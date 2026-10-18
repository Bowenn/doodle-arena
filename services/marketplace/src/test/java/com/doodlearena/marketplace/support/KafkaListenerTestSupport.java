package com.doodlearena.marketplace.support;

import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.context.annotation.Import;

/**
 * The consumers against a real broker and database. Listener test classes extend this, so they
 * share one Spring context, one Kafka and one Postgres.
 */
@SpringBootTest(properties = {
		"spring.kafka.listener.auto-startup=true",
		// Fail fast while Postgres is paused, so the error handler's retries are what the tests see.
		"spring.datasource.hikari.connection-timeout=2000",
		"spring.datasource.hikari.data-source-properties.socketTimeout=3" })
@Import({ PostgresTestConfiguration.class, KafkaTestConfiguration.class, LedgerFixtures.class })
public abstract class KafkaListenerTestSupport {
}
