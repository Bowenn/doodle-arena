package com.doodlearena.marketplace.kafka;

import java.util.HashMap;
import java.util.Map;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.kafka.autoconfigure.DefaultKafkaConsumerFactoryCustomizer;
import org.springframework.boot.kafka.autoconfigure.DefaultKafkaProducerFactoryCustomizer;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

/**
 * The marketplace's Kafka user (ADR 0013): SCRAM-SHA-512 with its own username and password,
 * over TLS when a CA is given (SASL_SSL in AWS). It may read game.matches.v1 and
 * assets.drawings.v1 in group marketplace and write the three market topics; the broker's ACLs
 * decide. With no username nothing is added, which is what the Testcontainers broker expects.
 */
@Configuration(proxyBeanMethods = false)
public class KafkaAuth {

	/** Client settings for a credential; empty without a username. */
	static Map<String, Object> properties(String username, String password, String caPem) {
		if (username.isEmpty()) {
			if (!caPem.isEmpty()) {
				throw new IllegalStateException("KAFKA_CA_PEM is set without KAFKA_USERNAME");
			}
			return Map.of();
		}
		if (password.isEmpty()) {
			throw new IllegalStateException("KAFKA_USERNAME is set without KAFKA_PASSWORD");
		}
		Map<String, Object> props = new HashMap<>();
		props.put("security.protocol", caPem.isEmpty() ? "SASL_PLAINTEXT" : "SASL_SSL");
		props.put("sasl.mechanism", "SCRAM-SHA-512");
		props.put("sasl.jaas.config", "org.apache.kafka.common.security.scram.ScramLoginModule required username=\""
				+ username + "\" password=\"" + password + "\";");
		if (!caPem.isEmpty()) {
			props.put("ssl.truststore.type", "PEM");
			props.put("ssl.truststore.certificates", caPem);
		}
		return props;
	}

	private final Map<String, Object> props;

	KafkaAuth(@Value("${KAFKA_USERNAME:}") String username, @Value("${KAFKA_PASSWORD:}") String password,
			@Value("${KAFKA_CA_PEM:}") String caPem) {
		this.props = properties(username, password, caPem);
	}

	@Bean
	DefaultKafkaProducerFactoryCustomizer kafkaProducerAuth() {
		return factory -> factory.updateConfigs(this.props);
	}

	@Bean
	DefaultKafkaConsumerFactoryCustomizer kafkaConsumerAuth() {
		return factory -> factory.updateConfigs(this.props);
	}

}
