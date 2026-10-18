package com.doodlearena.marketplace.stripe;

import java.time.Instant;
import java.util.UUID;

import com.jayway.jsonpath.JsonPath;
import com.stripe.net.Webhook;
import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.MediaType;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.StripeApiTestSupport;
import com.doodlearena.proto.doodle.events.v1.CreditReason;
import com.doodlearena.proto.doodle.events.v1.WalletEvent;

import static org.assertj.core.api.Assertions.assertThat;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/**
 * Stripe's webhook, with events built and signed in the test exactly as Stripe does it (an
 * HMAC-SHA256 of "timestamp.payload" with the endpoint's secret). Each test first starts a real
 * checkout against stripe-mock, so the session exists in stripe_checkouts.
 */
class StripeWebhookTests extends StripeApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private ReconciliationJob reconciliation;

	@Autowired
	private JdbcClient db;

	@Test
	void paidSessionIsCreditedOnce() throws Exception {
		String user = "u_" + UUID.randomUUID();
		String session = checkout(user, "medium");
		String event = completed("evt_" + UUID.randomUUID(), session, "paid", user);

		deliver(event).andExpect(status().isOk()).andExpect(jsonPath("$.result").value("processed"));

		assertThat(this.fixtures.balance(AccountRef.user(user))).isEqualTo(550);
		WalletEvent credited = WalletEvent.parseFrom(this.db
			.sql("SELECT payload FROM outbox WHERE topic = 'market.wallet.v1' AND key = ?")
			.param(user)
			.query(byte[].class)
			.single());
		assertThat(credited.getCoinsCredited().getAmount()).isEqualTo(550);
		assertThat(credited.getCoinsCredited().getReason()).isEqualTo(CreditReason.CREDIT_REASON_STRIPE_PURCHASE);
		assertThat(credited.getCoinsCredited().getReference()).isEqualTo(session);
		assertThat(this.reconciliation.reconcile().clean()).isTrue();
	}

	/** M4: replaying a Stripe webhook has no extra effect. */
	@Test
	void replayedEventHasNoExtraEffect() throws Exception {
		String user = "u_" + UUID.randomUUID();
		String session = checkout(user, "small");
		String event = completed("evt_" + UUID.randomUUID(), session, "paid", user);

		deliver(event).andExpect(jsonPath("$.result").value("processed"));
		deliver(event).andExpect(status().isOk()).andExpect(jsonPath("$.result").value("duplicate"));
		// A different event about the same paid session pays nothing either.
		deliver(event("evt_" + UUID.randomUUID(), StripeWebhookService.ASYNC_SUCCEEDED, session, "paid", user))
			.andExpect(jsonPath("$.result").value("duplicate"));

		assertThat(this.fixtures.balance(AccountRef.user(user))).isEqualTo(100);
		assertThat(this.db.sql("SELECT COUNT(*) FROM outbox WHERE topic = 'market.wallet.v1' AND key = ?")
			.param(user)
			.query(Long.class)
			.single()).isEqualTo(1);
	}

	@Test
	void unpaidOrOtherEventsCreditNothing() throws Exception {
		String user = "u_" + UUID.randomUUID();
		String session = checkout(user, "small");

		deliver(completed("evt_" + UUID.randomUUID(), session, "unpaid", user)).andExpect(jsonPath("$.result").value("ignored"));
		deliver(event("evt_" + UUID.randomUUID(), "payment_intent.created", session, null, user))
			.andExpect(jsonPath("$.result").value("ignored"));

		assertThat(this.fixtures.balance(AccountRef.user(user))).isZero();
	}

	@Test
	void sessionWeDidNotCreateIsNotCredited() throws Exception {
		String user = "u_" + UUID.randomUUID();
		deliver(completed("evt_" + UUID.randomUUID(), "cs_test_foreign", "paid", user)).andExpect(status().isOk())
			.andExpect(jsonPath("$.result").value("unknown_session"));
		// Someone else's session, with this user's id in the metadata.
		String session = checkout("u_" + UUID.randomUUID(), "small");
		deliver(completed("evt_" + UUID.randomUUID(), session, "paid", user)).andExpect(jsonPath("$.result").value("unknown_session"));

		assertThat(this.fixtures.balance(AccountRef.user(user))).isZero();
	}

	@Test
	void badSignaturesAreRefused() throws Exception {
		String user = "u_" + UUID.randomUUID();
		String session = checkout(user, "small");
		String event = completed("evt_" + UUID.randomUUID(), session, "paid", user);
		long now = Instant.now().getEpochSecond();

		send(event, "t=" + now + ",v1=" + sign(event, now, "whsec_wrong")).andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("INVALID_SIGNATURE"));
		send(event, null).andExpect(status().isBadRequest());
		send(event.replace("\"paid\"", "\"PAID\""), "t=" + now + ",v1=" + sign(event, now, WEBHOOK_SECRET))
			.andExpect(status().isBadRequest());
		// Signed correctly, but 10 minutes ago: a captured request can't be replayed later.
		long old = now - 600;
		send(event, "t=" + old + ",v1=" + sign(event, old, WEBHOOK_SECRET)).andExpect(status().isBadRequest());

		assertThat(this.fixtures.balance(AccountRef.user(user))).isZero();
	}

	private String checkout(String user, String pack) throws Exception {
		String body = this.mvc
			.perform(post("/api/market/coin-packs/" + pack + "/checkout").header("Authorization", CheckoutApiTests.bearer(user))
				.header("Idempotency-Key", UUID.randomUUID().toString()))
			.andExpect(status().isOk())
			.andReturn()
			.getResponse()
			.getContentAsString();
		return JsonPath.read(body, "$.session_id");
	}

	private static String completed(String eventId, String session, String paymentStatus, String user) {
		return event(eventId, StripeWebhookService.COMPLETED, session, paymentStatus, user);
	}

	/** An event shaped like Stripe's: only the fields the marketplace reads, plus the envelope. */
	private static String event(String eventId, String type, String session, String paymentStatus, String user) {
		return """
				{"id":"%s","object":"event","api_version":"2025-09-30.clover","created":%d,"livemode":false,
				"pending_webhooks":1,"request":{"id":null,"idempotency_key":null},"type":"%s",
				"data":{"object":{"id":"%s","object":"checkout.session","mode":"payment",%s
				"client_reference_id":"%s","metadata":{"user_id":"%s","pack_id":"small"}}}}"""
			.formatted(eventId, Instant.now().getEpochSecond(), type, session,
					paymentStatus == null ? "" : "\"payment_status\":\"" + paymentStatus + "\",", user, user);
	}

	private ResultActions deliver(String payload) throws Exception {
		long now = Instant.now().getEpochSecond();
		return send(payload, "t=" + now + ",v1=" + sign(payload, now, WEBHOOK_SECRET));
	}

	private ResultActions send(String payload, String signature) throws Exception {
		var request = post("/api/market/stripe/webhook").contentType(MediaType.APPLICATION_JSON).content(payload);
		if (signature != null) {
			request.header("Stripe-Signature", signature);
		}
		return this.mvc.perform(request);
	}

	private static String sign(String payload, long timestamp, String secret) throws Exception {
		return Webhook.Util.computeHmacSha256(secret, timestamp + "." + payload);
	}

}
