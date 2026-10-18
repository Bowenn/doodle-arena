package com.doodlearena.marketplace.stripe;

import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.support.StripeApiTestSupport;
import com.doodlearena.marketplace.support.TestTokens;

import static org.assertj.core.api.Assertions.assertThat;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.header;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/** Checkout against stripe-mock. */
class CheckoutApiTests extends StripeApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private JdbcClient db;

	@Test
	void checkoutStartsASessionAndRecordsTheSale() throws Exception {
		String user = "u_" + UUID.randomUUID();

		String body = checkout(user, "medium", "key-1").andExpect(status().isOk())
			.andExpect(jsonPath("$.session_id").isNotEmpty())
			.andExpect(jsonPath("$.checkout_url").isNotEmpty())
			.andReturn()
			.getResponse()
			.getContentAsString();

		var row = this.db.sql("SELECT session_id, pack_id, coins, amount_cents, currency FROM stripe_checkouts WHERE user_id = ?")
			.param(user)
			.query()
			.singleRow();
		assertThat(body).contains((String) row.get("session_id"));
		assertThat(row).containsEntry("pack_id", "medium")
			.containsEntry("coins", 550L)
			.containsEntry("amount_cents", 499L)
			.containsEntry("currency", "usd");
	}

	@Test
	void sameKeyReplaysTheSameSession() throws Exception {
		String user = "u_" + UUID.randomUUID();
		String first = checkout(user, "small", "key-2").andReturn().getResponse().getContentAsString();
		String second = checkout(user, "small", "key-2").andExpect(status().isOk())
			.andExpect(header().string("Idempotency-Replayed", "true"))
			.andReturn()
			.getResponse()
			.getContentAsString();

		assertThat(second).isEqualTo(first);
		assertThat(this.db.sql("SELECT COUNT(*) FROM stripe_checkouts WHERE user_id = ?").param(user).query(Long.class).single())
			.isEqualTo(1);
	}

	@Test
	void unknownPackAndMissingKeyAreRefused() throws Exception {
		String user = "u_" + UUID.randomUUID();
		checkout(user, "huge", "key-3").andExpect(status().isNotFound())
			.andExpect(jsonPath("$.error.code").value("COIN_PACK_NOT_FOUND"));
		this.mvc.perform(post("/api/market/coin-packs/small/checkout").header("Authorization", bearer(user)))
			.andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("IDEMPOTENCY_KEY_REQUIRED"));
	}

	private ResultActions checkout(String user, String pack, String key) throws Exception {
		return this.mvc.perform(post("/api/market/coin-packs/" + pack + "/checkout").header("Authorization", bearer(user))
			.header("Idempotency-Key", key));
	}

	static String bearer(String user) {
		return "Bearer " + TestTokens.valid(user);
	}

}
