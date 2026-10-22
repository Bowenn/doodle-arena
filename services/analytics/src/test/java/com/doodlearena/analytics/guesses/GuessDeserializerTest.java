package com.doodlearena.analytics.guesses;

import static org.junit.jupiter.api.Assertions.assertEquals;

import com.doodlearena.proto.doodle.events.v1.ViewerGuess;
import com.doodlearena.proto.doodle.events.v1.ViewerGuessEvent;
import com.google.protobuf.Timestamp;
import java.util.ArrayList;
import java.util.List;
import org.apache.flink.util.Collector;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.junit.jupiter.api.Test;

class GuessDeserializerTest {
	private static final Timestamp AT = Timestamp.newBuilder().setSeconds(1_800_000_000L).setNanos(250_000_000).build();

	private static List<Guess> read(byte[] value) {
		List<Guess> out = new ArrayList<>();
		new GuessDeserializer().deserialize(new ConsumerRecord<>("live.guesses.v1", 0, 7, new byte[0], value),
				new Collector<>() {
					@Override
					public void collect(Guess g) {
						out.add(g);
					}

					@Override
					public void close() {}
				});
		return out;
	}

	private static byte[] guess(String room, String text, boolean withTime) {
		ViewerGuess.Builder g = ViewerGuess.newBuilder().setRoomId(room).setRoundNo(2).setText(text);
		if (withTime) {
			g.setGuessedAt(AT);
		}
		return ViewerGuessEvent.newBuilder().setViewerGuess(g).build().toByteArray();
	}

	@Test
	void readsAGuessWithItsEventTime() {
		List<Guess> out = read(guess("r1", "cat", true));
		assertEquals(1, out.size());
		Guess g = out.get(0);
		assertEquals("r1", g.roomId);
		assertEquals(2, g.roundNo);
		assertEquals("cat", g.text);
		assertEquals(1_800_000_000_250L, g.guessedAtMs);
	}

	// ADR 0004: a record the job can't use is skipped, never a reason to fail.
	@Test
	void skipsWhatItCantUse() {
		assertEquals(0, read(new byte[] {(byte) 0xff, 0x01}).size(), "garbage bytes");
		assertEquals(0, read(ViewerGuessEvent.getDefaultInstance().toByteArray()).size(), "no case set");
		assertEquals(0, read(guess("r1", "Cat!", true)).size(), "text that isn't normalized");
		assertEquals(0, read(guess("", "cat", true)).size(), "no room");
		assertEquals(0, read(guess("r1", "cat", false)).size(), "no event time");
	}
}
