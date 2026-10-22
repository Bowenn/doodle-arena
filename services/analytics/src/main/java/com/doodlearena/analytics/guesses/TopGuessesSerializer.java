package com.doodlearena.analytics.guesses;

import com.doodlearena.proto.doodle.events.v1.TopGuesses;
import java.time.Instant;
import com.doodlearena.proto.doodle.events.v1.TopGuessesEvent;
import java.nio.charset.StandardCharsets;
import org.apache.flink.connector.kafka.sink.KafkaRecordSerializationSchema;
import org.apache.kafka.clients.producer.ProducerRecord;

/** Writes TopGuesses to live.top-guesses.v1, keyed by room_id like every live topic. */
public class TopGuessesSerializer implements KafkaRecordSerializationSchema<WindowTop> {
	private final String topic;

	public TopGuessesSerializer(String topic) {
		this.topic = topic;
	}

	@Override
	public ProducerRecord<byte[], byte[]> serialize(WindowTop w, KafkaSinkContext ctx, Long timestamp) {
		TopGuesses t = w.toEvent(Instant.now());
		byte[] value = TopGuessesEvent.newBuilder().setTopGuesses(t).build().toByteArray();
		return new ProducerRecord<>(topic, t.getRoomId().getBytes(StandardCharsets.UTF_8), value);
	}
}
