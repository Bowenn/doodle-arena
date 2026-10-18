package com.doodlearena.marketplace.stripe;

import com.stripe.StripeClient;

import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Component;

import com.doodlearena.marketplace.api.ApiException;

/** The Stripe API client, or a 503 when no test key is configured. */
@Component
public class StripeGateway {

	private final StripeClient client;

	public StripeGateway(StripeProperties properties) {
		this.client = properties.configured() ? StripeClient.builder()
			.setApiKey(properties.secretKey())
			.setApiBase(properties.apiBase())
			// A checkout request waits for Stripe; keep that wait short.
			.setConnectTimeout(5_000)
			.setReadTimeout(5_000)
			.setMaxNetworkRetries(1)
			.build() : null;
	}

	public StripeClient client() {
		if (this.client == null) {
			throw notConfigured();
		}
		return this.client;
	}

	static ApiException notConfigured() {
		return new ApiException(HttpStatus.SERVICE_UNAVAILABLE, "STRIPE_NOT_CONFIGURED",
				"coin packs are unavailable: Stripe isn't set up");
	}

}
