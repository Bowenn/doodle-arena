package com.doodlearena.marketplace.stripe;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.HexFormat;

import com.stripe.exception.StripeException;
import com.stripe.model.checkout.Session;
import com.stripe.net.RequestOptions;
import com.stripe.param.checkout.SessionCreateParams;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.idempotency.Claim;
import com.doodlearena.marketplace.stripe.StripeProperties.CoinPack;

/**
 * Starts a coin-pack purchase (design doc, section 7.7) in two steps of an idempotent request (ADR
 * 0012): {@link #createSession} calls Stripe with no transaction open, so a slow Stripe holds no
 * database connection; {@link #record} then stores what was sold, in the transaction that completes
 * the key. Stripe gets an idempotency key derived from ours, so a retried request that reaches
 * Stripe twice still gets the same session.
 */
@Service
public class CheckoutService {

	private static final Logger log = LoggerFactory.getLogger(CheckoutService.class);

	private final StripeProperties properties;

	private final StripeGateway stripe;

	private final JdbcClient db;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	public CheckoutService(StripeProperties properties, StripeGateway stripe, JdbcClient db) {
		this.properties = properties;
		this.stripe = stripe;
		this.db = db;
	}

	public CoinPack pack(String packId) {
		return this.properties.coinPacks()
			.stream()
			.filter((pack) -> pack.id().equals(packId))
			.findFirst()
			.orElseThrow(() -> new ApiException(HttpStatus.NOT_FOUND, "COIN_PACK_NOT_FOUND", "no such coin pack"));
	}

	public record Checkout(String sessionId, String checkoutUrl) {
	}

	/** The Stripe call. Runs outside any transaction (the caller holds no connection meanwhile). */
	@SuppressWarnings("try") // the scope only has to be open while Stripe is called
	public Checkout createSession(String userId, CoinPack pack, Claim claim) {
		Span span = this.tracer.spanBuilder("market.checkout")
			.setAttribute("user.id", userId)
			.setAttribute("coin_pack.id", pack.id())
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			String base = this.properties.publicBaseUrl();
			SessionCreateParams params = SessionCreateParams.builder()
				.setMode(SessionCreateParams.Mode.PAYMENT)
				.setSuccessUrl(base + "/wallet?checkout=success&session_id={CHECKOUT_SESSION_ID}")
				.setCancelUrl(base + "/wallet?checkout=cancelled")
				.setClientReferenceId(userId)
				.putMetadata("user_id", userId)
				.putMetadata("pack_id", pack.id())
				.addLineItem(SessionCreateParams.LineItem.builder()
					.setQuantity(1L)
					.setPriceData(SessionCreateParams.LineItem.PriceData.builder()
						.setCurrency(pack.currency())
						.setUnitAmount(pack.priceCents())
						.setProductData(SessionCreateParams.LineItem.PriceData.ProductData.builder()
							.setName(pack.coins() + " Doodle Arena coins")
							.build())
						.build())
					.build())
				.build();
			Session session;
			try {
				session = this.stripe.client()
					.v1()
					.checkout()
					.sessions()
					.create(params, RequestOptions.builder().setIdempotencyKey(stripeKey(claim)).build());
			}
			catch (StripeException ex) {
				log.atWarn()
					.addKeyValue("user_id", userId)
					.addKeyValue("stripe_error", ex.getClass().getSimpleName())
					.addKeyValue("stripe_request_id", ex.getRequestId())
					.log("Stripe refused or failed the checkout");
				throw new ApiException(HttpStatus.BAD_GATEWAY, "STRIPE_UNAVAILABLE", "Stripe couldn't start the checkout");
			}
			span.setAttribute("stripe.session_id", session.getId());
			return new Checkout(session.getId(), session.getUrl());
		}
		catch (RuntimeException ex) {
			span.recordException(ex);
			span.setStatus(StatusCode.ERROR);
			throw ex;
		}
		finally {
			span.end();
		}
	}

	/**
	 * Records the session the webhook will credit. Runs inside the transaction that completes the
	 * key: if the claim was taken over during the Stripe call, this insert rolls back, and the
	 * request that holds the key records the same session (Stripe returned it for the same key).
	 */
	public Checkout record(String userId, CoinPack pack, Checkout checkout) {
		this.db.sql("""
				INSERT INTO stripe_checkouts (session_id, user_id, pack_id, coins, amount_cents, currency)
				VALUES (?, ?, ?, ?, ?, ?)""")
			.params(checkout.sessionId(), userId, pack.id(), pack.coins(), pack.priceCents(), pack.currency())
			.update();
		log.atInfo()
			.addKeyValue("user_id", userId)
			.addKeyValue("pack_id", pack.id())
			.addKeyValue("stripe_session_id", checkout.sessionId())
			.log("checkout started");
		return checkout;
	}

	/** One Stripe idempotency key per user and our key; hashed so it says nothing about either. */
	private static String stripeKey(Claim claim) {
		try {
			MessageDigest digest = MessageDigest.getInstance("SHA-256");
			byte[] hash = digest.digest((claim.userId() + "\n" + claim.key()).getBytes(StandardCharsets.UTF_8));
			return "checkout-" + HexFormat.of().formatHex(hash);
		}
		catch (NoSuchAlgorithmException ex) {
			throw new IllegalStateException(ex);
		}
	}

}
