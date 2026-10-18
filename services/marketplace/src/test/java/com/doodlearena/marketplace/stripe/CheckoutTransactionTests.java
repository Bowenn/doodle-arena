package com.doodlearena.marketplace.stripe;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.mock.web.MockHttpServletResponse;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;
import org.springframework.test.context.TestPropertySource;
import org.springframework.test.web.servlet.MockMvc;

import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.TestTokens;

import static org.assertj.core.api.Assertions.assertThat;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;

/**
 * Task 4.R1: the checkout calls Stripe with no transaction open, so a slow Stripe holds no database
 * connection, and a claim lost during the call still records exactly one session. Stripe is a
 * local stub here (stripe-mock can't be slow or replay keys): like Stripe's API v1, it saves the
 * first response for each Idempotency-Key, a 500 included, and replays it.
 */
@TestPropertySource(properties = { "spring.datasource.hikari.maximum-pool-size=2",
		"spring.datasource.hikari.connection-timeout=1000", "app.idempotency.lease=1s" })
class CheckoutTransactionTests extends ApiTestSupport {

	private static final StripeStub STRIPE = new StripeStub();

	@DynamicPropertySource
	static void stripe(DynamicPropertyRegistry registry) {
		registry.add("app.stripe.secret-key", () -> "sk_test_123");
		registry.add("app.stripe.webhook-secret", () -> "whsec_unused");
		registry.add("app.stripe.api-base", STRIPE::url);
	}

	@Autowired
	private MockMvc mvc;

	@Autowired
	private JdbcClient db;

	@Autowired
	private ReconciliationJob reconciliation;

	@AfterEach
	void reset() {
		STRIPE.reset();
	}

	/** Before 4.R1, two held checkouts took the whole pool and everything else timed out. */
	@Test
	void slowStripeDoesNotHoldConnections() throws Exception {
		STRIPE.hold(5);
		try (ExecutorService pool = Executors.newFixedThreadPool(5)) {
			List<Future<MockHttpServletResponse>> checkouts = new ArrayList<>();
			for (int i = 0; i < 5; i++) {
				String user = "u_" + UUID.randomUUID();
				checkouts.add(pool.submit(() -> checkout(user, UUID.randomUUID().toString())));
			}
			assertThat(STRIPE.awaitHeld(5, 5, TimeUnit.SECONDS)).as("all five checkouts waiting in Stripe").isTrue();

			// With all five inside Stripe, the 2-connection pool still serves everything else.
			String reader = "u_" + UUID.randomUUID();
			assertThat(this.mvc.perform(get("/api/market/wallet").header("Authorization", bearer(reader)))
				.andReturn()
				.getResponse()
				.getStatus()).isEqualTo(200);
			assertThat(this.reconciliation.reconcile().clean()).isTrue();

			STRIPE.release();
			for (Future<MockHttpServletResponse> checkout : checkouts) {
				assertThat(checkout.get(10, TimeUnit.SECONDS).getStatus()).isEqualTo(200);
			}
		}
	}

	/**
	 * A request that waits in Stripe past its lease loses the key to its retry. The retry's claim
	 * gets its own Stripe key, so its own session; the first request's record rolls back with its
	 * fenced completion. One session is recorded, and every later answer is the retry's.
	 */
	@Test
	void claimTakenOverDuringTheStripeCallRecordsOnce() throws Exception {
		String user = "u_" + UUID.randomUUID();
		String key = UUID.randomUUID().toString();
		STRIPE.hold(1);
		try (ExecutorService pool = Executors.newSingleThreadExecutor()) {
			Future<MockHttpServletResponse> first = pool.submit(() -> checkout(user, key));
			assertThat(STRIPE.awaitHeld(1, 5, TimeUnit.SECONDS)).isTrue();
			Thread.sleep(1_500); // past the 1-second lease

			MockHttpServletResponse retry = checkout(user, key);
			assertThat(retry.getStatus()).isEqualTo(200);
			STRIPE.release();

			MockHttpServletResponse slow = first.get(10, TimeUnit.SECONDS);
			assertThat(slow.getStatus()).isEqualTo(409);
			assertThat(slow.getContentAsString()).contains("IDEMPOTENCY_KEY_IN_PROGRESS");

			assertThat(sessions(user)).hasSize(1);
			assertThat(retry.getContentAsString()).contains(sessions(user).getFirst());
			MockHttpServletResponse replay = checkout(user, key);
			assertThat(replay.getHeader("Idempotency-Replayed")).isEqualTo("true");
			assertThat(replay.getContentAsString()).isEqualTo(retry.getContentAsString());
		}
		assertThat(STRIPE.sessionsCreated()).isEqualTo(2); // the slow request's session is never recorded
	}

	/**
	 * Stripe replays a saved 500 for the same Stripe key for 24 hours, so a retry must not reuse it:
	 * the 502 releases our claim, and the retry's new claim asks Stripe with a new key.
	 */
	@Test
	void stripeFailureReleasesTheKeyAndTheRetryAsksAfresh() throws Exception {
		String user = "u_" + UUID.randomUUID();
		String key = UUID.randomUUID().toString();
		STRIPE.failing(true);
		MockHttpServletResponse failed = checkout(user, key);
		assertThat(failed.getStatus()).isEqualTo(502);
		assertThat(failed.getContentAsString()).contains("STRIPE_UNAVAILABLE");
		assertThat(sessions(user)).isEmpty();

		STRIPE.failing(false);
		MockHttpServletResponse retry = checkout(user, key);
		assertThat(retry.getStatus()).isEqualTo(200);
		assertThat(sessions(user)).hasSize(1);
	}

	private MockHttpServletResponse checkout(String user, String key) throws Exception {
		return this.mvc
			.perform(post("/api/market/coin-packs/small/checkout").header("Authorization", bearer(user))
				.header("Idempotency-Key", key))
			.andReturn()
			.getResponse();
	}

	private List<String> sessions(String user) {
		return this.db.sql("SELECT session_id FROM stripe_checkouts WHERE user_id = ?").param(user).query(String.class).list();
	}

	private static String bearer(String user) {
		return "Bearer " + TestTokens.valid(user);
	}

	/**
	 * POST /v1/checkout/sessions, answered like Stripe's API v1: the first response for an
	 * Idempotency-Key is saved and replayed for that key, a 500 included. Can hold the next n
	 * requests until released, and fail new keys with a 500.
	 */
	static final class StripeStub {

		private final HttpServer server;

		private final Map<String, Saved> saved = new ConcurrentHashMap<>();

		private final AtomicInteger created = new AtomicInteger();

		private volatile CountDownLatch gate = new CountDownLatch(0);

		private final AtomicInteger toHold = new AtomicInteger();

		private final AtomicInteger held = new AtomicInteger();

		private volatile boolean failing;

		private record Saved(int status, String body) {
		}

		StripeStub() {
			try {
				this.server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
			}
			catch (IOException ex) {
				throw new IllegalStateException(ex);
			}
			this.server.setExecutor(Executors.newCachedThreadPool());
			this.server.createContext("/v1/checkout/sessions", this::handle);
			this.server.start();
		}

		String url() {
			return "http://127.0.0.1:" + this.server.getAddress().getPort();
		}

		void hold(int requests) {
			this.gate = new CountDownLatch(1);
			this.toHold.set(requests);
			this.held.set(0);
		}

		boolean awaitHeld(int requests, long timeout, TimeUnit unit) throws InterruptedException {
			long deadline = System.nanoTime() + unit.toNanos(timeout);
			while (this.held.get() < requests) {
				if (System.nanoTime() > deadline) {
					return false;
				}
				Thread.sleep(10);
			}
			return true;
		}

		void release() {
			this.gate.countDown();
		}

		void failing(boolean failing) {
			this.failing = failing;
		}

		int sessionsCreated() {
			return this.created.get();
		}

		void reset() {
			release();
			this.toHold.set(0);
			this.failing = false;
			this.created.set(0);
		}

		private void handle(HttpExchange exchange) throws IOException {
			try (exchange) {
				exchange.getRequestBody().readAllBytes();
				if (this.toHold.getAndDecrement() > 0) {
					this.held.incrementAndGet();
					try {
						// Well under the client's 5-second read timeout.
						this.gate.await(4, TimeUnit.SECONDS);
					}
					catch (InterruptedException ex) {
						Thread.currentThread().interrupt();
					}
				}
				String key = exchange.getRequestHeaders().getFirst("Idempotency-Key");
				Saved answer = this.saved.computeIfAbsent(key, (k) -> this.failing
						? new Saved(500, "{\"error\":{\"type\":\"api_error\",\"message\":\"stub failure\"}}")
						: session());
				byte[] body = answer.body().getBytes(StandardCharsets.UTF_8);
				exchange.getResponseHeaders().set("Content-Type", "application/json");
				exchange.getResponseHeaders().set("Idempotency-Key", key);
				exchange.sendResponseHeaders(answer.status(), body.length);
				exchange.getResponseBody().write(body);
			}
		}

		private Saved session() {
			this.created.incrementAndGet();
			String id = "cs_test_" + UUID.randomUUID().toString().replace("-", "");
			return new Saved(200, "{\"id\":\"" + id + "\",\"object\":\"checkout.session\",\"url\":\"https://checkout.stripe.test/"
					+ id + "\"}");
		}

	}

}
