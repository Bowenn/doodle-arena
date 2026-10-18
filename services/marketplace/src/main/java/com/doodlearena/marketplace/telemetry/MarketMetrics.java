package com.doodlearena.marketplace.telemetry;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.metrics.DoubleHistogram;
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

	private final Meter meter = GlobalOpenTelemetry.getMeter("marketplace");

	private final DoubleHistogram lockWait = this.meter.histogramBuilder("market.db.lock_wait.duration")
		.setUnit("ms")
		.setDescription("Time spent waiting for row locks, per operation")
		.build();

	public Meter meter() {
		return this.meter;
	}

	public void recordLockWait(String operation, long nanos) {
		this.lockWait.record(nanos / 1_000_000.0, Attributes.of(OPERATION, operation));
	}

}
