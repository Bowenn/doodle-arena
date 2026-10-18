package com.doodlearena.marketplace.support;

import org.testcontainers.containers.GenericContainer;
import org.testcontainers.containers.wait.strategy.Wait;
import org.testcontainers.utility.DockerImageName;

import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;

/**
 * The API with Stripe configured against stripe-mock, Stripe's official API simulator, so the
 * requests we build are checked against Stripe's real API shapes. Webhook tests sign their events
 * with {@link #WEBHOOK_SECRET}, the same way Stripe does.
 */
public abstract class StripeApiTestSupport extends ApiTestSupport {

	public static final String WEBHOOK_SECRET = "whsec_test_doodle_arena";

	@SuppressWarnings("resource") // stopped by Testcontainers' reaper when the JVM exits
	private static final GenericContainer<?> STRIPE_MOCK = new GenericContainer<>(
			DockerImageName.parse("stripe/stripe-mock:v0.206.0"))
		.withExposedPorts(12111)
		.waitingFor(Wait.forListeningPort());

	static {
		STRIPE_MOCK.start();
	}

	@DynamicPropertySource
	static void stripe(DynamicPropertyRegistry registry) {
		registry.add("app.stripe.secret-key", () -> "sk_test_123");
		registry.add("app.stripe.webhook-secret", () -> WEBHOOK_SECRET);
		registry.add("app.stripe.api-base",
				() -> "http://" + STRIPE_MOCK.getHost() + ":" + STRIPE_MOCK.getMappedPort(12111));
	}

}
