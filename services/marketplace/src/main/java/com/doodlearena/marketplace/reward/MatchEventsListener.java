package com.doodlearena.marketplace.reward;

import com.google.protobuf.InvalidProtocolBufferException;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Component;

import com.doodlearena.marketplace.reward.RewardService.RewardResult;
import com.doodlearena.marketplace.telemetry.MarketMetrics;
import com.doodlearena.proto.doodle.events.v1.MatchEvent;

/**
 * Pays rewards for finished matches: consumes game.matches.v1 (design doc, sections 6.2 and 7.9).
 * The same rules as the drawings consumer: the offset is committed after the payment committed; a
 * record this build can't read, or one that breaks its contract, is logged, counted and committed;
 * anything else is retried on the same record by the error handler, so no match goes unpaid
 * because Postgres was down.
 */
@Component
public class MatchEventsListener {

	public static final String TOPIC = "game.matches.v1";

	private static final Logger log = LoggerFactory.getLogger(MatchEventsListener.class);

	private final RewardService rewards;

	private final MarketMetrics metrics;

	public MatchEventsListener(RewardService rewards, MarketMetrics metrics) {
		this.rewards = rewards;
		this.metrics = metrics;
	}

	// idIsGroup = false: the consumer group is spring.kafka.consumer.group-id ("marketplace"), not the id.
	@KafkaListener(id = "match-events", idIsGroup = false, topics = TOPIC)
	public void onRecord(ConsumerRecord<String, byte[]> record) {
		MatchEvent event;
		try {
			if (record.value() == null) {
				throw new InvalidProtocolBufferException("empty record");
			}
			event = MatchEvent.parseFrom(record.value());
		}
		catch (InvalidProtocolBufferException ex) {
			skip(record, "undecodable match event skipped");
			return;
		}
		if (event.getEventCase() != MatchEvent.EventCase.MATCH_FINISHED) {
			// An unset case, or one added after this build (its field is unknown here).
			skip(record, "unknown match event skipped");
			return;
		}
		RewardRequest request;
		try {
			request = RewardRequest.from(event.getMatchFinished());
		}
		catch (InvalidMatchEventException ex) {
			log.atError()
				.addKeyValue("match_id", event.getMatchFinished().getMatchId())
				.addKeyValue("partition", record.partition())
				.addKeyValue("offset", record.offset())
				.addKeyValue("reason", ex.getMessage())
				.log("match event breaks its contract; skipped");
			this.metrics.matchReceived("failed");
			return;
		}
		RewardResult result = this.rewards.pay(request);
		this.metrics.matchReceived(switch (result) {
			case PAID -> "paid";
			case DUPLICATE -> "duplicate";
			case INELIGIBLE -> "ineligible";
		});
	}

	private void skip(ConsumerRecord<String, byte[]> record, String message) {
		log.atWarn().addKeyValue("partition", record.partition()).addKeyValue("offset", record.offset()).log(message);
		this.metrics.matchReceived("skipped");
	}

}
