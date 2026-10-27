package com.doodlearena.marketplace.security;

import java.io.IOException;
import java.net.MalformedURLException;
import java.net.URI;
import java.net.URISyntaxException;
import java.time.Duration;
import java.util.function.BiConsumer;

import com.nimbusds.jose.KeySourceException;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jose.jwk.source.JWKSetCacheRefreshEvaluator;
import com.nimbusds.jose.jwk.source.JWKSetSource;
import com.nimbusds.jose.jwk.source.JWKSource;
import com.nimbusds.jose.jwk.source.JWKSourceBuilder;
import com.nimbusds.jose.jwk.source.URLBasedJWKSetSource;
import com.nimbusds.jose.proc.SecurityContext;
import com.nimbusds.jose.util.DefaultResourceRetriever;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.metrics.LongCounter;
import io.opentelemetry.api.metrics.Meter;

/**
 * Auth's JWKS, under the rule every service shares (design doc 4.2, libs/go/authn): the key set is
 * used for {@code maxAge} (10 minutes), so a key auth withdraws stops working within it; an
 * unknown key id refetches, but fetches are rate-limited to {@code minRefresh} (30 s), so forged
 * key ids can't make this service hammer auth; and while auth can't be reached the last good set
 * stays in use until {@code maxStale} (an hour) after the last successful fetch, then every token
 * is refused until a fetch succeeds.
 *
 * <p>Built from Nimbus's own sources rather than a port of the Go code. Two differences from it:
 * Nimbus's rate limiter allows two fetches per window, not one, and a request that arrives when the
 * set has expired waits for the refresh (at most the fetch timeout, 3 seconds); refresh-ahead keeps
 * that rare by refreshing in the background during the set's last 30 seconds.
 *
 * <p>Nimbus caches the set an outage serves again for a full {@code maxAge}, so outage tolerance is
 * {@code maxStale - maxAge}: a refresh that fails just before it ends keeps the old set until
 * {@code maxStale} at most, never longer. The grace therefore ends somewhere between
 * {@code maxStale - maxAge} and {@code maxStale} after the last good fetch, not exactly at
 * {@code maxStale} as in Go.
 */
final class JwksSource {

	private static final int MAX_DOCUMENT_BYTES = 1 << 20; // as in Go

	private static final AttributeKey<String> REASON = AttributeKey.stringKey("reason");
	private static final AttributeKey<String> RESULT = AttributeKey.stringKey("result");

	/**
	 * The rule's three settings and the three that follow from them. Nimbus requires
	 * {@code refreshAhead + refreshWait <= maxAge}, and {@code refreshWait} should outlast one fetch.
	 */
	record Timings(Duration maxAge, Duration minRefresh, Duration maxStale, Duration fetchTimeout,
			Duration refreshAhead, Duration refreshWait) {

		Timings {
			if (maxStale.compareTo(maxAge) <= 0) {
				throw new IllegalArgumentException("jwks max-stale must be longer than max-age");
			}
			if (minRefresh.compareTo(maxAge) >= 0) {
				throw new IllegalArgumentException("jwks min-refresh must be shorter than max-age");
			}
		}

		/** Production: a 3-second fetch, refreshed in the set's last 30 seconds, waited on up to 5. */
		static Timings of(Duration maxAge, Duration minRefresh, Duration maxStale) {
			return new Timings(maxAge, minRefresh, maxStale, Duration.ofSeconds(3), Duration.ofSeconds(30),
					Duration.ofSeconds(5));
		}

	}

	private JwksSource() {
	}

	static JWKSource<SecurityContext> create(String jwksUrl, Timings timings, Meter meter) {
		LongCounter fetches = meter.counterBuilder("authn.jwks.fetches")
			.setUnit("{fetch}")
			.setDescription("JWKS fetches by reason (max_age, unknown_kid) and result (success, failed)")
			.build();
		return create(jwksUrl, timings, (reason, result) -> fetches.add(1, Attributes.of(REASON, reason, RESULT, result)));
	}

	/** The same, reporting each fetch to {@code onFetch(reason, result)}: for tests. */
	static JWKSource<SecurityContext> create(String jwksUrl, Timings t, BiConsumer<String, String> onFetch) {
		int timeoutMs = Math.toIntExact(t.fetchTimeout().toMillis());
		DefaultResourceRetriever http = new DefaultResourceRetriever(timeoutMs, timeoutMs, MAX_DOCUMENT_BYTES);
		JWKSetSource<SecurityContext> fetch;
		try {
			fetch = new URLBasedJWKSetSource<>(new URI(jwksUrl).toURL(), http);
		}
		catch (URISyntaxException | MalformedURLException | IllegalArgumentException ex) {
			throw new IllegalArgumentException("invalid JWKS URL: " + jwksUrl, ex);
		}
		return JWKSourceBuilder.create(new Counting(fetch, t.maxAge().minus(t.refreshAhead()), onFetch))
			.cache(t.maxAge().toMillis(), t.refreshWait().toMillis())
			.refreshAheadCache(t.refreshAhead().toMillis(), false)
			.rateLimited(t.minRefresh().toMillis())
			.outageTolerant(t.maxStale().minus(t.maxAge()).toMillis())
			.retrying(false)
			.build();
	}

	/**
	 * Reports every fetch. Nimbus refreshes for an unknown key id and for an old set through the
	 * same evaluator, so the reason comes from the set's age, as the rule defines it: a fetch before
	 * the set is due is for an unknown key id.
	 */
	private static final class Counting implements JWKSetSource<SecurityContext> {

		private final JWKSetSource<SecurityContext> fetch;
		private final long dueMs;
		private final BiConsumer<String, String> onFetch;
		private volatile long lastSuccessMs = -1;

		Counting(JWKSetSource<SecurityContext> fetch, Duration due, BiConsumer<String, String> onFetch) {
			this.fetch = fetch;
			this.dueMs = due.toMillis();
			this.onFetch = onFetch;
		}

		@Override
		public JWKSet getJWKSet(JWKSetCacheRefreshEvaluator refreshEvaluator, long currentTime,
				SecurityContext context) throws KeySourceException {
			long last = this.lastSuccessMs;
			String reason = (last < 0 || currentTime - last >= this.dueMs) ? "max_age" : "unknown_kid";
			try {
				JWKSet set = this.fetch.getJWKSet(refreshEvaluator, currentTime, context);
				this.lastSuccessMs = currentTime;
				this.onFetch.accept(reason, "success");
				return set;
			}
			catch (KeySourceException | RuntimeException ex) {
				this.onFetch.accept(reason, "failed");
				throw ex;
			}
		}

		@Override
		public void close() throws IOException {
			this.fetch.close();
		}

	}

}
