package com.doodlearena.analytics.guesses;

import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.GuessCount;
import com.doodlearena.proto.doodle.events.v1.TopGuesses;
import com.google.protobuf.Timestamp;
import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.UUID;

/**
 * One window's top guesses inside the job. A Flink POJO rather than the protobuf message: Flink's
 * generic serializer can't copy protobuf's immutable lists, so the message is built only at the
 * Kafka sink (toEvent).
 */
public class WindowTop {
	public String roomId;
	public int roundNo;
	public long windowEndMs;
	/** Most common first; texts.get(i) was guessed counts.get(i) times. */
	public List<String> texts = new ArrayList<>();
	public List<Integer> counts = new ArrayList<>();

	public WindowTop() {}

	/** The live.top-guesses.v1 message. */
	public TopGuesses toEvent(Instant now) {
		TopGuesses.Builder b = TopGuesses.newBuilder()
				.setMeta(EventMeta.newBuilder()
						// From the room, round and window: a window emitted again after a restart
						// (at-least-once, ADR 0018) carries the same id.
						.setEventId(UUID.nameUUIDFromBytes((roomId + "/" + roundNo + "/" + windowEndMs)
								.getBytes(StandardCharsets.UTF_8)).toString())
						.setEventType("TopGuesses")
						.setSchemaVersion(1)
						.setOccurredAt(ts(now.toEpochMilli()))
						.setProducer("analytics"))
				.setRoomId(roomId)
				.setRoundNo(roundNo)
				.setWindowEnd(ts(windowEndMs));
		for (int i = 0; i < texts.size(); i++) {
			b.addGuesses(GuessCount.newBuilder().setText(texts.get(i)).setCount(counts.get(i)));
		}
		return b.build();
	}

	private static Timestamp ts(long ms) {
		return Timestamp.newBuilder().setSeconds(Math.floorDiv(ms, 1000)).setNanos(Math.floorMod(ms, 1000) * 1_000_000).build();
	}
}
