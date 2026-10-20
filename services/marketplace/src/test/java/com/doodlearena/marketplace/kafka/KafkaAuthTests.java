package com.doodlearena.marketplace.kafka;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import org.junit.jupiter.api.Test;

class KafkaAuthTests {

	@Test
	void noUsernameAddsNothing() {
		assertThat(KafkaAuth.properties("", "", "")).isEmpty();
	}

	@Test
	void usernameAddsScram() {
		assertThat(KafkaAuth.properties("marketplace", "secret", ""))
			.containsEntry("security.protocol", "SASL_PLAINTEXT")
			.containsEntry("sasl.mechanism", "SCRAM-SHA-512")
			.containsEntry("sasl.jaas.config",
					"org.apache.kafka.common.security.scram.ScramLoginModule required username=\"marketplace\" password=\"secret\";")
			.doesNotContainKey("ssl.truststore.certificates");
	}

	@Test
	void caAddsTls() {
		assertThat(KafkaAuth.properties("marketplace", "secret", "pem"))
			.containsEntry("security.protocol", "SASL_SSL")
			.containsEntry("ssl.truststore.type", "PEM")
			.containsEntry("ssl.truststore.certificates", "pem");
	}

	@Test
	void incompleteConfigIsAnError() {
		assertThatThrownBy(() -> KafkaAuth.properties("marketplace", "", "")).isInstanceOf(IllegalStateException.class);
		assertThatThrownBy(() -> KafkaAuth.properties("", "", "pem")).isInstanceOf(IllegalStateException.class);
	}

}
