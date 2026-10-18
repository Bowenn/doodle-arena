package com.doodlearena.marketplace.ownership;

import com.google.protobuf.InvalidProtocolBufferException;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Component;

import com.doodlearena.marketplace.ownership.MintService.MintResult;
import com.doodlearena.marketplace.telemetry.MarketMetrics;
import com.doodlearena.proto.doodle.events.v1.DrawingEvent;

/**
 * Mints what assets approves: consumes assets.drawings.v1 (design doc, sections 6.2 and 7.6).
 *
 * <ul>
 * <li>Delivery is at least once (assets' outbox relay), so minting is idempotent by drawing id.</li>
 * <li>The offset is committed after this method returns, so after the mint committed (ack mode
 * RECORD). A crash in between redelivers the record, which is then a duplicate.</li>
 * <li>A record this build can't read (undecodable, or an unset or unknown event, ADR 0004) is
 * logged, counted as skipped and committed. A readable event that breaks its contract is logged at
 * error, counted as failed and committed: retrying can't fix either, and neither may block the
 * partition.</li>
 * <li>Anything else, such as Postgres being down, is thrown to the error handler, which retries the
 * same record until it succeeds ({@link KafkaConfig}).</li>
 * </ul>
 */
@Component
public class DrawingEventsListener {

	public static final String TOPIC = "assets.drawings.v1";

	private static final Logger log = LoggerFactory.getLogger(DrawingEventsListener.class);

	private final MintService mints;

	private final MarketMetrics metrics;

	public DrawingEventsListener(MintService mints, MarketMetrics metrics) {
		this.mints = mints;
		this.metrics = metrics;
	}

	// idIsGroup = false: the consumer group is spring.kafka.consumer.group-id ("marketplace"), not the id.
	@KafkaListener(id = "drawing-events", idIsGroup = false, topics = TOPIC)
	public void onRecord(ConsumerRecord<String, byte[]> record) {
		DrawingEvent event;
		try {
			if (record.value() == null) {
				throw new InvalidProtocolBufferException("empty record");
			}
			event = DrawingEvent.parseFrom(record.value());
		}
		catch (InvalidProtocolBufferException ex) {
			skip(record, "undecodable drawing event skipped");
			return;
		}
		switch (event.getEventCase()) {
			case DRAWING_APPROVED -> approved(record, event);
			case DRAWING_REJECTED -> {
				log.atDebug().addKeyValue("drawing_id", event.getDrawingRejected().getDrawingId()).log("rejected drawing ignored");
				this.metrics.drawingReceived("rejected");
			}
			// An unset case, or one added after this build (its field is unknown here).
			default -> skip(record, "unknown drawing event skipped");
		}
	}

	private void approved(ConsumerRecord<String, byte[]> record, DrawingEvent event) {
		MintRequest request;
		try {
			request = MintRequest.from(event.getDrawingApproved());
		}
		catch (InvalidDrawingEventException ex) {
			log.atError()
				.addKeyValue("drawing_id", event.getDrawingApproved().getDrawingId())
				.addKeyValue("partition", record.partition())
				.addKeyValue("offset", record.offset())
				.addKeyValue("reason", ex.getMessage())
				.log("drawing event breaks its contract; skipped");
			this.metrics.drawingReceived("failed");
			return;
		}
		MintResult result = this.mints.mint(request);
		this.metrics.drawingReceived(result == MintResult.MINTED ? "minted" : "duplicate");
	}

	private void skip(ConsumerRecord<String, byte[]> record, String message) {
		log.atWarn().addKeyValue("partition", record.partition()).addKeyValue("offset", record.offset()).log(message);
		this.metrics.drawingReceived("skipped");
	}

}
