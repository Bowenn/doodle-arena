package com.doodlearena.marketplace.outbox;

import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.TimeUnit;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import io.opentelemetry.context.propagation.TextMapGetter;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.kafka.core.KafkaTemplate;
import org.springframework.kafka.support.SendResult;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * Publishes the outbox to Kafka (design doc, section 8). Each pass locks a batch of unpublished
 * rows with {@code FOR UPDATE SKIP LOCKED} (so several instances share the work without sending a
 * row twice at the same time), sends them, waits for Kafka's acknowledgements and marks the
 * acknowledged rows published in the same transaction. A row whose send failed stays for the next
 * pass. A crash after Kafka acknowledged but before the commit sends the row again with the same
 * {@code event_id}: delivery is at least once, and consumers dedupe.
 *
 * <p>Each send runs under the trace context stored with the row, so the consumer's spans join the
 * request that wrote the event.
 */
@Component
public class OutboxRelay {

	private static final Logger log = LoggerFactory.getLogger(OutboxRelay.class);

	private static final int BATCH = 100;

	private static final Duration ACK_TIMEOUT = Duration.ofSeconds(10);

	private static final TextMapGetter<Map<String, String>> GETTER = new TextMapGetter<>() {

		@Override
		public Iterable<String> keys(Map<String, String> carrier) {
			return carrier.keySet();
		}

		@Override
		public String get(Map<String, String> carrier, String key) {
			return (carrier == null) ? null : carrier.get(key);
		}

	};

	private final JdbcClient db;

	private final TransactionTemplate tx;

	private final KafkaTemplate<String, byte[]> kafka;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	private final boolean enabled;

	private final Duration retention;

	public OutboxRelay(JdbcClient db, PlatformTransactionManager transactions, KafkaTemplate<String, byte[]> kafka,
			MarketMetrics metrics, @Value("${app.outbox.relay-enabled:true}") boolean enabled,
			@Value("${app.outbox.retention:7d}") Duration retention) {
		this.db = db;
		this.enabled = enabled;
		this.retention = retention;
		this.tx = new TransactionTemplate(transactions);
		this.kafka = kafka;
		metrics.meter()
			.gaugeBuilder("market.outbox.lag")
			.setUnit("s")
			.setDescription("Age of the oldest event Kafka hasn't acknowledged; 0 when the outbox is drained")
			.buildWithCallback((gauge) -> {
				try {
					gauge.record(lagSeconds());
				}
				catch (RuntimeException ex) {
					// No value rather than a wrong one while the database can't be read.
				}
			});
	}

	/**
	 * Deletes events Kafka acknowledged more than the retention ago (7 days by default): by then
	 * they're on the topics, and the rows only grow the table. Unpublished rows are never touched.
	 */
	@Scheduled(fixedDelayString = "${app.outbox.cleanup-interval:1h}")
	public int deletePublished() {
		int deleted = this.db.sql("DELETE FROM outbox WHERE published_at < now() - make_interval(secs => ?)")
			.param(this.retention.toSeconds())
			.update();
		if (deleted > 0) {
			log.atInfo().addKeyValue("deleted", deleted).log("published outbox events deleted");
		}
		return deleted;
	}

	@Scheduled(fixedDelayString = "${app.outbox.interval}")
	public void run() {
		if (!this.enabled) {
			return;
		}
		try {
			publishBatch();
		}
		catch (RuntimeException ex) {
			log.atWarn().setCause(ex).log("outbox pass failed");
		}
	}

	/** One pass; returns how many events Kafka acknowledged. */
	public int publishBatch() {
		Integer published = this.tx.execute((status) -> {
			List<Row> rows = this.db.sql("""
					SELECT event_id, topic, key, payload, traceparent FROM outbox
					WHERE published_at IS NULL ORDER BY created_at LIMIT ? FOR UPDATE SKIP LOCKED""")
				.param(BATCH)
				.query((rs, i) -> new Row(rs.getObject("event_id", UUID.class), rs.getString("topic"), rs.getString("key"),
						rs.getBytes("payload"), rs.getString("traceparent")))
				.list();
			if (rows.isEmpty()) {
				return 0;
			}
			List<CompletableFuture<SendResult<String, byte[]>>> sends = new ArrayList<>();
			for (Row row : rows) {
				sends.add(send(row));
			}
			List<UUID> acked = new ArrayList<>();
			long deadline = System.nanoTime() + ACK_TIMEOUT.toNanos();
			for (int i = 0; i < rows.size(); i++) {
				try {
					sends.get(i).get(Math.max(0, deadline - System.nanoTime()), TimeUnit.NANOSECONDS);
					acked.add(rows.get(i).eventId());
				}
				catch (Exception ex) {
					log.atWarn()
						.addKeyValue("event_id", rows.get(i).eventId())
						.addKeyValue("topic", rows.get(i).topic())
						.addKeyValue("error", ex.getClass().getSimpleName())
						.log("outbox event not acknowledged; retried on the next pass");
				}
			}
			if (!acked.isEmpty()) {
				this.db.sql("UPDATE outbox SET published_at = now() WHERE event_id IN (:ids)").param("ids", acked).update();
			}
			return acked.size();
		});
		return (published != null) ? published : 0;
	}

	@SuppressWarnings("try") // the scope only has to be open while the send starts
	private CompletableFuture<SendResult<String, byte[]>> send(Row row) {
		Context parent = GlobalOpenTelemetry.getPropagators()
			.getTextMapPropagator()
			.extract(Context.root(), (row.traceparent() == null) ? Map.of() : Map.of("traceparent", row.traceparent()),
					GETTER);
		Span span = this.tracer.spanBuilder("market.outbox.publish")
			.setParent(parent)
			.setSpanKind(SpanKind.INTERNAL)
			.setAttribute("messaging.destination.name", row.topic())
			.setAttribute("event.id", row.eventId().toString())
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			// The agent's producer span starts under this one and puts its traceparent on the record.
			return this.kafka.send(new ProducerRecord<>(row.topic(), row.key(), row.payload()))
				.whenComplete((result, ex) -> span.end());
		}
		catch (RuntimeException ex) {
			span.end();
			return CompletableFuture.failedFuture(ex);
		}
	}

	private double lagSeconds() {
		Double lag = this.db.sql("""
				SELECT COALESCE(EXTRACT(EPOCH FROM now() - MIN(created_at)), 0)::float8
				FROM outbox WHERE published_at IS NULL""")
			.query(Double.class)
			.single();
		return (lag != null) ? lag : 0;
	}

	private record Row(UUID eventId, String topic, String key, byte[] payload, String traceparent) {
	}

}
