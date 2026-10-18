package com.doodlearena.marketplace.stripe;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.test.web.servlet.MockMvc;

import com.doodlearena.marketplace.support.ApiTestSupport;

import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/** Coin packs in the default test context, where no Stripe key is configured. */
class CoinPackApiTests extends ApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Test
	void packsAreListed() throws Exception {
		this.mvc.perform(get("/api/market/coin-packs").header("Authorization", CheckoutApiTests.bearer("u_reader")))
			.andExpect(status().isOk())
			.andExpect(jsonPath("$.packs.length()").value(3))
			.andExpect(jsonPath("$.packs[0].pack_id").value("small"))
			.andExpect(jsonPath("$.packs[0].coins").value(100))
			.andExpect(jsonPath("$.packs[0].price_cents").value(99))
			.andExpect(jsonPath("$.packs[0].currency").value("usd"));
	}

	@Test
	void withoutAStripeKeyCheckoutAndWebhookAreUnavailable() throws Exception {
		this.mvc.perform(post("/api/market/coin-packs/small/checkout")
			.header("Authorization", CheckoutApiTests.bearer("u_reader"))
			.header("Idempotency-Key", "k"))
			.andExpect(status().isServiceUnavailable())
			.andExpect(jsonPath("$.error.code").value("STRIPE_NOT_CONFIGURED"));
		this.mvc.perform(post("/api/market/stripe/webhook").header("Stripe-Signature", "t=1,v1=x").content("{}"))
			.andExpect(status().isServiceUnavailable())
			.andExpect(jsonPath("$.error.code").value("STRIPE_NOT_CONFIGURED"));
	}

}
