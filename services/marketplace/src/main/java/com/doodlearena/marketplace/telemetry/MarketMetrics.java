package com.doodlearena.marketplace.telemetry;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.metrics.DoubleHistogram;
import io.opentelemetry.api.metrics.LongCounter;
import io.opentelemetry.api.metrics.Meter;

import org.springframework.stereotype.Component;

/**
 * The marketplace's custom metrics (docs/observability.md, section 3), through the OpenTelemetry
 * API. The Java agent supplies the SDK and exports them; without the agent (tests) they are no-ops.
 * Ids are never attributes.
 */
@Component
public class MarketMetrics {

	private static final AttributeKey<String> OPERATION = AttributeKey.stringKey("operation");

	private static final AttributeKey<String> RESULT = AttributeKey.stringKey("result");

	private static final AttributeKey<String> TRIGGER = AttributeKey.stringKey("trigger");

	private final Meter meter = GlobalOpenTelemetry.getMeter("marketplace");

	private final DoubleHistogram lockWait = this.meter.histogramBuilder("market.db.lock_wait.duration")
		.setUnit("ms")
		.setDescription("Time spent waiting for row locks, per operation")
		.build();

	private final LongCounter drawingsReceived = this.meter.counterBuilder("market.drawings.received")
		.setDescription("Records read from assets.drawings.v1, by outcome")
		.build();

	private final LongCounter idempotencyReplays = this.meter.counterBuilder("market.idempotency.replays")
		.setDescription("Requests answered from a stored response, per operation")
		.build();

	private final LongCounter purchases = this.meter.counterBuilder("market.purchases")
		.setDescription("Purchase attempts, by outcome")
		.build();

	private final LongCounter matchesReceived = this.meter.counterBuilder("market.matches.received")
		.setDescription("Records read from game.matches.v1, by outcome")
		.build();

	private final LongCounter stripeWebhooks = this.meter.counterBuilder("market.stripe.webhooks")
		.setDescription("Stripe webhook requests, by outcome")
		.build();

	private final LongCounter tips = this.meter.counterBuilder("market.tips")
		.setDescription("Tip requests, by outcome")
		.build();

	private final LongCounter tipsClosed = this.meter.counterBuilder("market.tips.settled")
		.setDescription("Tips taken out of escrow: paid to the drawers or refunded, by what closed them")
		.build();

	private final LongCounter roundsReceived = this.meter.counterBuilder("market.rounds.received")
		.setDescription("Records read from game.rounds.v1, by outcome")
		.build();

	public Meter meter() {
		return this.meter;
	}

	/** One record from assets.drawings.v1: minted, duplicate, rejected, skipped or failed. */
	public void drawingReceived(String result) {
		this.drawingsReceived.add(1, Attributes.of(RESULT, result));
	}

	/** One record from game.matches.v1: paid, duplicate, ineligible, skipped or failed. */
	public void matchReceived(String result) {
		this.matchesReceived.add(1, Attributes.of(RESULT, result));
	}

	/** processed, duplicate, ignored, unknown_session or invalid_signature. */
	public void stripeWebhook(String result) {
		this.stripeWebhooks.add(1, Attributes.of(RESULT, result));
	}

	/** success, conflict (the listing was sold or cancelled) or insufficient_funds. */
	public void purchase(String result) {
		this.purchases.add(1, Attributes.of(RESULT, result));
	}

	/** accepted, insufficient_funds or round_over. */
	public void tip(String result) {
		this.tips.add(1, Attributes.of(RESULT, result));
	}

	/** {@code count} tips closed: result paid or refunded, trigger round (its RoundFinished) or sweep. */
	public void tipsClosed(long count, String result, String trigger) {
		if (count > 0) {
			this.tipsClosed.add(count, Attributes.of(RESULT, result, TRIGGER, trigger));
		}
	}

	/** One record from game.rounds.v1: settled, skipped or failed. */
	public void roundReceived(String result) {
		this.roundsReceived.add(1, Attributes.of(RESULT, result));
	}

	public void idempotencyReplay(String operation) {
		this.idempotencyReplays.add(1, Attributes.of(OPERATION, operation));
	}

	public void recordLockWait(String operation, long nanos) {
		this.lockWait.record(nanos / 1_000_000.0, Attributes.of(OPERATION, operation));
	}

}
