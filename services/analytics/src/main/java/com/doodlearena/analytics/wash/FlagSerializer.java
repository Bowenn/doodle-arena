package com.doodlearena.analytics.wash;

import com.doodlearena.proto.doodle.events.v1.AnalyticsFlagEvent;
import java.nio.charset.StandardCharsets;
import java.time.Instant;
import org.apache.flink.connector.kafka.sink.KafkaRecordSerializationSchema;
import org.apache.kafka.clients.producer.ProducerRecord;

/** Writes WashTradeFlagged to analytics.flags.v1, keyed by drawing_id like market.sales.v1. */
public class FlagSerializer implements KafkaRecordSerializationSchema<Flag> {
	private final String topic;

	public FlagSerializer(String topic) {
		this.topic = topic;
	}

	@Override
	public ProducerRecord<byte[], byte[]> serialize(Flag f, KafkaSinkContext ctx, Long timestamp) {
		byte[] value = AnalyticsFlagEvent.newBuilder().setWashTradeFlagged(f.toEvent(Instant.now())).build().toByteArray();
		return new ProducerRecord<>(topic, f.drawingId.getBytes(StandardCharsets.UTF_8), value);
	}
}
