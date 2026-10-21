package com.doodlearena.marketplace.stripe;

import java.util.List;

import org.junit.jupiter.api.Test;

import static org.assertj.core.api.Assertions.assertThat;

/** When Stripe counts as set up: checkout and the webhook answer 503 otherwise. */
class StripePropertiesTests {

	private static StripeProperties withKey(String secretKey) {
		return new StripeProperties(secretKey, "whsec_test", null, "http://localhost:8080", List.of());
	}

	@Test
	void aTestKeyIsConfigured() {
		assertThat(withKey("sk_test_123").configured()).isTrue();
	}

	@Test
	void noKeyAndTheAwsPlaceholderAreNotConfigured() {
		// In AWS an SSM parameter can't be empty: one nobody has put yet holds "unset".
		assertThat(withKey(null).configured()).isFalse();
		assertThat(withKey(" ").configured()).isFalse();
		assertThat(withKey("unset").configured()).isFalse();
	}

}
