package com.doodlearena.analytics.guesses;

import com.doodlearena.proto.doodle.events.v1.ViewerGuess;
import com.doodlearena.proto.doodle.events.v1.ViewerGuessEvent;
import com.google.protobuf.InvalidProtocolBufferException;
import java.util.regex.Pattern;
import org.apache.flink.api.common.typeinfo.TypeInformation;
import org.apache.flink.connector.kafka.source.reader.deserializer.KafkaRecordDeserializationSchema;
import org.apache.flink.metrics.Counter;
import org.apache.flink.util.Collector;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Reads live.guesses.v1. An unset or unknown case, bytes that don't decode or a guess that isn't
 * normalized is logged with its topic, partition and offset, counted and skipped (ADR 0004): one
 * bad record must never stop the job.
 */
public class GuessDeserializer implements KafkaRecordDeserializationSchema<Guess> {
	private static final Logger LOG = LoggerFactory.getLogger(GuessDeserializer.class);
	// What the relays publish: the game's normalized form (design doc 6.4).
	private static final Pattern NORMALIZED = Pattern.compile("[a-z]{1,100}");

	private transient Counter read;
	private transient Counter skipped;

	@Override
	public void open(org.apache.flink.api.common.serialization.DeserializationSchema.InitializationContext context) {
		read = context.getMetricGroup().counter("analytics.guesses.read");
		skipped = context.getMetricGroup().counter("analytics.guesses.skipped");
	}

	@Override
	public void deserialize(ConsumerRecord<byte[], byte[]> record, Collector<Guess> out) {
		ViewerGuess g;
		try {
			ViewerGuessEvent e = ViewerGuessEvent.parseFrom(record.value());
			if (e.getEventCase() != ViewerGuessEvent.EventCase.VIEWER_GUESS) {
				skip(record, "unset or unknown case " + e.getEventCase());
				return;
			}
			g = e.getViewerGuess();
		} catch (InvalidProtocolBufferException ex) {
			skip(record, "not a ViewerGuessEvent: " + ex.getMessage());
			return;
		}
		if (g.getRoomId().isEmpty() || !NORMALIZED.matcher(g.getText()).matches() || !g.hasGuessedAt()) {
			skip(record, "a guess without its room, time or normalized text");
			return;
		}
		if (read != null) {
			read.inc();
		}
		long ms = g.getGuessedAt().getSeconds() * 1000 + g.getGuessedAt().getNanos() / 1_000_000;
		out.collect(new Guess(g.getRoomId(), g.getRoundNo(), g.getText(), ms));
	}

	private void skip(ConsumerRecord<byte[], byte[]> record, String why) {
		if (skipped != null) {
			skipped.inc();
		}
		LOG.warn("skipped a record: {} (topic {}, partition {}, offset {})", why, record.topic(), record.partition(),
				record.offset());
	}

	@Override
	public TypeInformation<Guess> getProducedType() {
		return TypeInformation.of(Guess.class);
	}
}
