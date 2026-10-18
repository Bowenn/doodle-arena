package com.doodlearena.marketplace.stripe;

import java.util.List;

import org.springframework.boot.context.properties.ConfigurationProperties;

/**
 * Stripe settings and the coin packs for sale (design doc, section 7.7). The keys are test-mode
 * keys from the environment; without a secret key, checkout and the webhook answer 503.
 * @param secretKey the API key (sk_test_...), empty when Stripe isn't set up
 * @param webhookSecret the signing secret of the webhook endpoint (printed by {@code stripe listen})
 * @param apiBase Stripe's API address; tests point it at stripe-mock
 * @param publicBaseUrl where the browser comes back to after Checkout
 */
@ConfigurationProperties("app.stripe")
public record StripeProperties(String secretKey, String webhookSecret, String apiBase, String publicBaseUrl,
		List<CoinPack> coinPacks) {

	public StripeProperties {
		coinPacks = (coinPacks == null) ? List.of() : List.copyOf(coinPacks);
	}

	public boolean configured() {
		return this.secretKey != null && !this.secretKey.isBlank();
	}

	/** A pack for sale: coins for a price in the currency's smallest unit (cents). */
	public record CoinPack(String id, long coins, long priceCents, String currency) {
	}

}
