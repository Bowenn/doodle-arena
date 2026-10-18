package com.doodlearena.marketplace.outbox;

import java.time.Instant;
import java.util.HashMap;
import java.util.Map;
import java.util.UUID;

import com.google.protobuf.Timestamp;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.context.Context;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Component;
import org.springframework.transaction.annotation.Propagation;
import org.springframework.transaction.annotation.Transactional;

import com.doodlearena.proto.doodle.events.v1.EventMeta;

/**
 * Writes an event into the outbox inside the caller's transaction (design doc, section 8): the
 * event exists exactly when the change it describes committed. {@link OutboxRelay} publishes it.
 */
@Component
public class OutboxWriter {

	public static final String LISTINGS_TOPIC = "market.listings.v1";

	public static final String SALES_TOPIC = "market.sales.v1";

	public static final String WALLET_TOPIC = "market.wallet.v1";

	private final JdbcClient db;

	public OutboxWriter(JdbcClient db) {
		this.db = db;
	}

	/** A fresh event id with its metadata (ADR 0004); the outbox row reuses the id. */
	public static EventMeta meta(String eventType, Instant occurredAt) {
		return EventMeta.newBuilder()
			.setEventId(UUID.randomUUID().toString())
			.setEventType(eventType)
			.setSchemaVersion(1)
			.setOccurredAt(timestamp(occurredAt))
			.setProducer("marketplace")
			.build();
	}

	public static Timestamp timestamp(Instant instant) {
		return Timestamp.newBuilder().setSeconds(instant.getEpochSecond()).setNanos(instant.getNano()).build();
	}

	@Transactional(propagation = Propagation.MANDATORY)
	public void write(String topic, String key, EventMeta meta, byte[] payload) {
		this.db.sql("INSERT INTO outbox (event_id, topic, key, payload, traceparent) VALUES (?, ?, ?, ?, ?)")
			.params(UUID.fromString(meta.getEventId()), topic, key, payload, traceparent())
			.update();
	}

	/** The current span's W3C trace context, so the consumer's spans join this request's trace. */
	private static String traceparent() {
		Map<String, String> carrier = new HashMap<>();
		GlobalOpenTelemetry.getPropagators().getTextMapPropagator().inject(Context.current(), carrier, Map::put);
		return carrier.get("traceparent");
	}

}
