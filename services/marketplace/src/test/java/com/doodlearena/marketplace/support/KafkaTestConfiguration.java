package com.doodlearena.marketplace.support;

import org.apache.kafka.clients.admin.NewTopic;
import org.testcontainers.kafka.KafkaContainer;
import org.testcontainers.utility.DockerImageName;

import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.boot.testcontainers.service.connection.ServiceConnection;
import org.springframework.context.annotation.Bean;

import com.doodlearena.marketplace.outbox.OutboxWriter;
import com.doodlearena.marketplace.ownership.DrawingEventsListener;

/** A real Kafka broker for listener tests, the same image as docker-compose. */
@TestConfiguration(proxyBeanMethods = false)
public class KafkaTestConfiguration {

	@Bean
	@ServiceConnection
	KafkaContainer kafka() {
		return new KafkaContainer(DockerImageName.parse("apache/kafka:4.2.2"));
	}

	/** Created before the listeners start, so they don't wait for a metadata refresh to see it. */
	@Bean
	NewTopic drawingsTopic() {
		return new NewTopic(DrawingEventsListener.TOPIC, 1, (short) 1);
	}

	@Bean
	NewTopic salesTopic() {
		return new NewTopic(OutboxWriter.SALES_TOPIC, 1, (short) 1);
	}

	@Bean
	NewTopic listingsTopic() {
		return new NewTopic(OutboxWriter.LISTINGS_TOPIC, 1, (short) 1);
	}

}
