package com.doodlearena.analytics.guesses;

import static org.junit.jupiter.api.Assertions.assertEquals;

import com.doodlearena.proto.doodle.events.v1.TopGuesses;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.stream.Collectors;
import org.apache.flink.api.common.eventtime.WatermarkStrategy;
import org.apache.flink.runtime.testutils.MiniClusterResourceConfiguration;
import org.apache.flink.streaming.api.datastream.DataStream;
import org.apache.flink.streaming.api.environment.StreamExecutionEnvironment;
import org.apache.flink.table.api.bridge.java.StreamTableEnvironment;
import org.apache.flink.test.junit5.MiniClusterExtension;
import org.apache.flink.util.CloseableIterator;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.RegisterExtension;

/** The query on a local mini-cluster, with event time from the guesses (rule: .claude/rules/java.md). */
class TopGuessesQueryTest {
	@RegisterExtension
	static final MiniClusterExtension MINI = new MiniClusterExtension(new MiniClusterResourceConfiguration.Builder()
			.setNumberTaskManagers(1).setNumberSlotsPerTaskManager(2).build());

	private static List<TopGuesses> run(List<Guess> in, Duration window, Duration slide, int topN) throws Exception {
		StreamExecutionEnvironment env = StreamExecutionEnvironment.getExecutionEnvironment();
		env.setParallelism(1);
		StreamTableEnvironment tEnv = StreamTableEnvironment.create(env);
		DataStream<Guess> guesses = env.fromData(in).assignTimestampsAndWatermarks(
				WatermarkStrategy.<Guess>forMonotonousTimestamps().withTimestampAssigner((g, ts) -> g.guessedAtMs));
		List<TopGuesses> out = new ArrayList<>();
		try (CloseableIterator<WindowTop> it = TopGuessesQuery.apply(tEnv, guesses, window, slide, topN).executeAndCollect()) {
			it.forEachRemaining(w -> out.add(w.toEvent(java.time.Instant.EPOCH)));
		}
		return out;
	}

	private static String ranked(TopGuesses t) {
		return t.getGuessesList().stream().map(c -> c.getText() + "=" + c.getCount()).collect(Collectors.joining(" "));
	}

	@Test
	void ranksAWindowsGuessesAndKeepsTheTopN() throws Exception {
		List<Guess> in = new ArrayList<>();
		// One room, one round, all within the first second: dog 3, cat 2, cow 2, fox 1.
		long t0 = 1_800_000_000_000L;
		for (String text : List.of("dog", "cat", "dog", "cow", "fox", "cat", "dog", "cow")) {
			in.add(new Guess("r1", 1, text, t0 + in.size() * 10));
		}
		List<TopGuesses> out = run(in, Duration.ofSeconds(10), Duration.ofSeconds(10), 3);
		assertEquals(1, out.size(), "one tumbling window");
		TopGuesses t = out.get(0);
		assertEquals("r1", t.getRoomId());
		assertEquals(1, t.getRoundNo());
		// Ties are broken by text, so the list is the same on every replay.
		assertEquals("dog=3 cat=2 cow=2", ranked(t));
		assertEquals(t0 / 1000 - (t0 / 1000) % 10 + 10, t.getWindowEnd().getSeconds());
		assertEquals("TopGuesses", t.getMeta().getEventType());
	}

	@Test
	void keepsRoomsAndRoundsApartAndSlides() throws Exception {
		long t0 = 1_800_000_000_000L; // a multiple of 10 s
		List<Guess> in = List.of(
				new Guess("r1", 1, "cat", t0 + 1_000),
				new Guess("r2", 1, "dog", t0 + 1_000),
				new Guess("r1", 2, "sun", t0 + 2_000),
				new Guess("r1", 1, "cat", t0 + 6_000));
		// 10 s windows sliding every 5 s: [t0-5, t0+5), [t0, t0+10), [t0+5, t0+15).
		List<TopGuesses> out = run(in, Duration.ofSeconds(10), Duration.ofSeconds(5), 5);
		List<String> got = out.stream()
				.map(t -> t.getRoomId() + "/" + t.getRoundNo() + "@" + (t.getWindowEnd().getSeconds() - t0 / 1000) + " " + ranked(t))
				.sorted().toList();
		assertEquals(List.of(
				"r1/1@10 cat=2",
				"r1/1@15 cat=1",
				"r1/1@5 cat=1",
				"r1/2@10 sun=1",
				"r1/2@5 sun=1",
				"r2/1@10 dog=1",
				"r2/1@5 dog=1"), got);
	}

	@Test
	void aRepeatedWindowHasTheSameEventId() throws Exception {
		long t0 = 1_800_000_000_000L;
		List<Guess> in = List.of(new Guess("r1", 1, "cat", t0 + 1_000));
		String first = run(in, Duration.ofSeconds(10), Duration.ofSeconds(10), 5).get(0).getMeta().getEventId();
		String again = run(in, Duration.ofSeconds(10), Duration.ofSeconds(10), 5).get(0).getMeta().getEventId();
		assertEquals(first, again, "at-least-once: a window emitted again after a restart is recognizable");
	}

}
