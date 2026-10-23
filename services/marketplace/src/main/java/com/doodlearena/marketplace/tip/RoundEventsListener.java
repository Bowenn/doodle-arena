package com.doodlearena.marketplace.tip;

import com.google.protobuf.InvalidProtocolBufferException;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.kafka.annotation.KafkaListener;
import org.springframework.stereotype.Component;

import com.doodlearena.marketplace.telemetry.MarketMetrics;
import com.doodlearena.proto.doodle.events.v1.RoundEvent;

/**
 * Settles tips when their round finishes: consumes game.rounds.v1 (ADR 0021). The same rules as
 * the other consumers: the offset is committed after the settlement committed; a record this
 * build can't read, or one that breaks its contract, is logged, counted and committed; anything
 * else is retried on the same record by the error handler. A redelivered round pays nothing more.
 */
@Component
public class RoundEventsListener {

	public static final String TOPIC = "game.rounds.v1";

	private static final Logger log = LoggerFactory.getLogger(RoundEventsListener.class);

	private final TipSettlement settlement;

	private final MarketMetrics metrics;

	public RoundEventsListener(TipSettlement settlement, MarketMetrics metrics) {
		this.settlement = settlement;
		this.metrics = metrics;
	}

	// idIsGroup = false: the consumer group is spring.kafka.consumer.group-id ("marketplace"), not the id.
	@KafkaListener(id = "round-events", idIsGroup = false, topics = TOPIC)
	public void onRecord(ConsumerRecord<String, byte[]> record) {
		RoundEvent event;
		try {
			if (record.value() == null) {
				throw new InvalidProtocolBufferException("empty record");
			}
			event = RoundEvent.parseFrom(record.value());
		}
		catch (InvalidProtocolBufferException ex) {
			skip(record, "undecodable round event skipped");
			return;
		}
		if (event.getEventCase() != RoundEvent.EventCase.ROUND_FINISHED) {
			// An unset case, or one added after this build (its field is unknown here).
			skip(record, "unknown round event skipped");
			return;
		}
		RoundRequest request;
		try {
			request = RoundRequest.from(event.getRoundFinished());
		}
		catch (InvalidRoundEventException ex) {
			log.atError()
				.addKeyValue("match_id", event.getRoundFinished().getMatchId())
				.addKeyValue("partition", record.partition())
				.addKeyValue("offset", record.offset())
				.addKeyValue("reason", ex.getMessage())
				.log("round event breaks its contract; skipped");
			this.metrics.roundReceived("failed");
			return;
		}
		this.settlement.roundFinished(request);
		this.metrics.roundReceived("settled");
	}

	private void skip(ConsumerRecord<String, byte[]> record, String message) {
		log.atWarn().addKeyValue("partition", record.partition()).addKeyValue("offset", record.offset()).log(message);
		this.metrics.roundReceived("skipped");
	}

}
