package com.doodlearena.marketplace.security;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Date;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

import com.nimbusds.jose.JOSEException;
import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.crypto.RSASSASigner;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jose.jwk.RSAKey;
import com.nimbusds.jose.jwk.gen.RSAKeyGenerator;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import org.springframework.security.oauth2.jose.jws.SignatureAlgorithm;
import org.springframework.security.oauth2.jwt.JwtDecoder;
import org.springframework.security.oauth2.jwt.JwtException;
import org.springframework.security.oauth2.jwt.NimbusJwtDecoder;

/**
 * The shared JWKS rule (design doc 4.2) as the marketplace builds it from Nimbus's sources, against
 * a fake auth that can change its keys, fail, or hang. Real time, with the rule shrunk to seconds:
 * every bound is checked by polling with a deadline and leaves at least twice the margin it needs.
 */
class JwksSourceTests {

	private static final JwksSource.Timings TIMINGS = new JwksSource.Timings(Duration.ofSeconds(1),
			Duration.ofMillis(300), Duration.ofSeconds(3), Duration.ofMillis(300), Duration.ofMillis(200),
			Duration.ofMillis(500));

	private final FakeAuth auth = new FakeAuth();

	private final List<String> fetches = new CopyOnWriteArrayList<>();

	private JwtDecoder decoder;

	@BeforeEach
	void start() throws IOException {
		this.auth.start();
		this.decoder = NimbusJwtDecoder
			.withJwkSource(JwksSource.create(this.auth.url(), TIMINGS, (reason, result) -> this.fetches.add(reason + ":" + result)))
			.jwsAlgorithm(SignatureAlgorithm.RS256)
			.build();
	}

	@AfterEach
	void stop() {
		this.auth.stop();
	}

	@Test
	void aWithdrawnKeyIsRefusedOnceTheSetIsOld() throws Exception {
		RSAKey a = key("a");
		RSAKey b = key("b");
		this.auth.keys(a);
		assertThat(verifies(token(a))).isTrue();

		this.auth.keys(b); // auth withdraws a
		assertThat(verifies(token(a))).as("the cached set still has a").isTrue();
		eventually(Duration.ofSeconds(3), () -> !verifies(token(a)));
		assertThat(verifies(token(b))).isTrue();
	}

	@Test
	void theLastGoodSetOutlivesAnOutageUntilMaxStale() throws Exception {
		RSAKey a = key("a");
		this.auth.keys(a);
		assertThat(verifies(token(a))).isTrue();
		Instant fetched = Instant.now();

		this.auth.failing(true);
		// Past max-age, auth down: the old set is still used.
		sleepUntil(fetched.plus(TIMINGS.maxAge()).plusMillis(500));
		assertThat(verifies(token(a))).as("within max-stale of the last good fetch").isTrue();
		assertThat(this.fetches).contains("max_age:failed");
		// Past max-stale: every token is refused until a fetch succeeds.
		eventually(Duration.between(Instant.now(), fetched.plus(TIMINGS.maxStale()).plusSeconds(2)),
				() -> !verifies(token(a)));

		this.auth.failing(false);
		eventually(Duration.ofSeconds(3), () -> verifies(token(a)));
	}

	@Test
	void unknownKeyIdsCantMakeItHammerAuth() throws Exception {
		RSAKey a = key("a");
		this.auth.keys(a);
		assertThat(verifies(token(a))).isTrue();
		int before = this.auth.hits();

		long started = System.nanoTime();
		for (int i = 0; i < 50; i++) {
			assertThat(verifies(token(key(UUID.randomUUID().toString())))).isFalse();
		}
		Duration burst = Duration.ofNanos(System.nanoTime() - started);

		// Nimbus's limiter lets two fetches through per min-refresh window.
		long windows = burst.toMillis() / TIMINGS.minRefresh().toMillis() + 1;
		assertThat(this.auth.hits() - before).isLessThanOrEqualTo((int) (2 * windows));
		assertThat(this.fetches).contains("unknown_kid:success");
	}

	@Test
	void aKeyAuthRotatesInIsFetchedForItsFirstToken() throws Exception {
		RSAKey a = key("a");
		RSAKey c = key("c");
		this.auth.keys(a);
		assertThat(verifies(token(a))).isTrue();

		this.auth.keys(a, c);
		Thread.sleep(TIMINGS.minRefresh().toMillis() + 100);
		assertThat(verifies(token(c))).isTrue();
		assertThat(verifies(token(a))).isTrue();
	}

	@Test
	void aHangingAuthCostsAtMostOneFetchTimeout() throws Exception {
		RSAKey a = key("a");
		this.auth.keys(a);
		assertThat(verifies(token(a))).isTrue();
		Instant fetched = Instant.now();

		this.auth.hanging(true);
		sleepUntil(fetched.plus(TIMINGS.maxAge()).plusMillis(200));
		long started = System.nanoTime();
		assertThat(verifies(token(a))).as("the last good set, after the fetch times out").isTrue();
		assertThat(Duration.ofNanos(System.nanoTime() - started))
			.isLessThan(TIMINGS.fetchTimeout().multipliedBy(2).plus(TIMINGS.refreshWait()));
	}

	@Test
	void theFirstFetchIsCountedAsMaxAge() throws Exception {
		RSAKey a = key("a");
		this.auth.keys(a);
		assertThat(verifies(token(a))).isTrue();
		assertThat(this.fetches).containsExactly("max_age:success");
	}

	@Test
	void timingsRefuseARuleThatCantHold() {
		assertThatThrownBy(() -> JwksSource.Timings.of(Duration.ofMinutes(10), Duration.ofSeconds(30),
				Duration.ofMinutes(10)))
			.isInstanceOf(IllegalArgumentException.class);
		assertThatThrownBy(() -> JwksSource.Timings.of(Duration.ofMinutes(10), Duration.ofMinutes(10),
				Duration.ofHours(1)))
			.isInstanceOf(IllegalArgumentException.class);
	}

	private boolean verifies(String token) {
		try {
			this.decoder.decode(token);
			return true;
		}
		catch (JwtException ex) {
			return false;
		}
	}

	private static RSAKey key(String kid) throws JOSEException {
		return new RSAKeyGenerator(2048).keyID(kid).generate();
	}

	private static String token(RSAKey key) throws JOSEException {
		Instant now = Instant.now();
		SignedJWT jwt = new SignedJWT(new JWSHeader.Builder(JWSAlgorithm.RS256).keyID(key.getKeyID()).build(),
				new JWTClaimsSet.Builder().subject("u1")
					.issueTime(Date.from(now))
					.expirationTime(Date.from(now.plusSeconds(60)))
					.build());
		jwt.sign(new RSASSASigner(key));
		return jwt.serialize();
	}

	private static void sleepUntil(Instant when) throws InterruptedException {
		long ms = Duration.between(Instant.now(), when).toMillis();
		if (ms > 0) {
			Thread.sleep(ms);
		}
	}

	/** Polls {@code condition} every 50 ms until it holds or {@code within} has passed. */
	private static void eventually(Duration within, Check condition) throws Exception {
		Instant deadline = Instant.now().plus(within);
		while (!condition.holds()) {
			assertThat(Instant.now()).as("condition still false at the deadline").isBefore(deadline);
			Thread.sleep(50);
		}
	}

	@FunctionalInterface
	private interface Check {

		boolean holds() throws Exception;

	}

	/** Auth's JWKS endpoint: serves the given public keys, or answers 503, or never answers in time. */
	private static final class FakeAuth {

		private final AtomicReference<String> document = new AtomicReference<>("{\"keys\":[]}");

		private final AtomicInteger hits = new AtomicInteger();

		private volatile boolean failing;

		private volatile boolean hanging;

		private HttpServer server;

		void start() throws IOException {
			this.server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
			this.server.setExecutor(Executors.newCachedThreadPool());
			this.server.createContext("/jwks.json", (exchange) -> {
				this.hits.incrementAndGet();
				if (this.hanging) {
					try {
						Thread.sleep(2_000);
					}
					catch (InterruptedException ex) {
						Thread.currentThread().interrupt();
					}
				}
				byte[] body = this.document.get().getBytes(StandardCharsets.UTF_8);
				int status = this.failing ? 503 : 200;
				exchange.sendResponseHeaders(status, body.length);
				try (OutputStream out = exchange.getResponseBody()) {
					out.write(body);
				}
			});
			this.server.start();
		}

		void stop() {
			this.server.stop(0);
		}

		String url() {
			return "http://127.0.0.1:" + this.server.getAddress().getPort() + "/jwks.json";
		}

		void keys(RSAKey... keys) {
			List<com.nimbusds.jose.jwk.JWK> pub = new ArrayList<>();
			for (RSAKey k : keys) {
				pub.add(k.toPublicJWK());
			}
			this.document.set(new JWKSet(pub).toString());
		}

		void failing(boolean failing) {
			this.failing = failing;
		}

		void hanging(boolean hanging) {
			this.hanging = hanging;
		}

		int hits() {
			return this.hits.get();
		}

	}

}
