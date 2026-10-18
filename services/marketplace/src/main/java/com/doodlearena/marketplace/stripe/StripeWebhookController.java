package com.doodlearena.marketplace.stripe;

import com.stripe.exception.SignatureVerificationException;
import com.stripe.net.Webhook;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import tools.jackson.core.JacksonException;
import tools.jackson.databind.JsonNode;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.http.HttpStatus;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestHeader;
import org.springframework.web.bind.annotation.RestController;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * Stripe's webhook endpoint (design doc, section 7.7). There is no JWT: the Stripe-Signature header
 * (an HMAC-SHA256 of the timestamp and the raw body with the endpoint's secret) proves the request
 * came from Stripe, and a timestamp older than 5 minutes is refused, so a captured request can't be
 * replayed later. Everything after that answers 2xx quickly.
 */
@RestController
public class StripeWebhookController {

	private static final long TOLERANCE_SECONDS = 300;

	private static final Logger log = LoggerFactory.getLogger(StripeWebhookController.class);

	private final StripeProperties properties;

	private final StripeWebhookService webhooks;

	private final JsonMapper json;

	private final MarketMetrics metrics;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	public StripeWebhookController(StripeProperties properties, StripeWebhookService webhooks, JsonMapper json,
			MarketMetrics metrics) {
		this.properties = properties;
		this.webhooks = webhooks;
		this.json = json;
		this.metrics = metrics;
	}

	@PostMapping("/api/market/stripe/webhook")
	@SuppressWarnings("try") // the scope only has to be open while the event is handled
	public WebhookAnswer receive(@RequestBody(required = false) String payload,
			@RequestHeader(name = "Stripe-Signature", required = false) String signature) {
		String secret = this.properties.webhookSecret();
		if (secret == null || secret.isBlank()) {
			throw StripeGateway.notConfigured();
		}
		try {
			Webhook.constructEvent(payload == null ? "" : payload, signature, secret, TOLERANCE_SECONDS);
		}
		catch (SignatureVerificationException | RuntimeException ex) {
			this.metrics.stripeWebhook("invalid_signature");
			log.atWarn().addKeyValue("error", ex.getClass().getSimpleName()).log("Stripe webhook refused: bad signature");
			throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_SIGNATURE", "the Stripe signature doesn't verify");
		}
		StripeWebhookService.VerifiedEvent event = parse(payload);
		Span span = this.tracer.spanBuilder("market.stripe.webhook")
			.setAttribute("stripe.event_type", event.type())
			.setAttribute("stripe.event_id", event.eventId())
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			String result = this.webhooks.handle(event);
			span.setAttribute("stripe.result", result);
			this.metrics.stripeWebhook(result);
			return new WebhookAnswer(true, result);
		}
		finally {
			span.end();
		}
	}

	/**
	 * Reads the few fields we need straight from the verified JSON, so the answer doesn't depend on
	 * which Stripe API version the event was rendered in.
	 */
	private StripeWebhookService.VerifiedEvent parse(String payload) {
		try {
			JsonNode root = this.json.readTree(payload);
			JsonNode object = root.path("data").path("object");
			return new StripeWebhookService.VerifiedEvent(root.path("id").asString(), root.path("type").asString(),
					object.path("object").asString().equals("checkout.session") ? object.path("id").asString() : null,
					object.path("payment_status").asString(null), object.path("metadata").path("user_id").asString(null));
		}
		catch (JacksonException ex) {
			throw ApiException.invalidRequest("unreadable Stripe event");
		}
	}

	public record WebhookAnswer(boolean received, String result) {
	}

}
