package com.doodlearena.analytics.wash;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.doodlearena.proto.doodle.events.v1.WashTradeFlagged;
import com.doodlearena.proto.doodle.events.v1.WashTradeRule;
import java.time.Duration;
import java.time.Instant;
import java.util.List;
import org.apache.flink.runtime.checkpoint.OperatorSubtaskState;
import org.apache.flink.streaming.api.operators.KeyedProcessOperator;
import org.apache.flink.streaming.util.KeyedOneInputStreamOperatorTestHarness;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** The detector in Flink's keyed operator harness, with event time and checkpoints. */
class WashTradeDetectorTest {
	private static final long T0 = 1_800_000_000_000L;
	private static final long HOUR = 3_600_000L;
	private static final Duration WEEK = Duration.ofDays(7);

	private KeyedOneInputStreamOperatorTestHarness<String, Sale, Flag> harness;

	private static KeyedOneInputStreamOperatorTestHarness<String, Sale, Flag> newHarness() throws Exception {
		return new KeyedOneInputStreamOperatorTestHarness<>(new KeyedProcessOperator<>(new WashTradeDetector(WEEK)),
				s -> s.drawingId, org.apache.flink.api.common.typeinfo.Types.STRING);
	}

	@BeforeEach
	void open() throws Exception {
		harness = newHarness();
		harness.open();
	}

	@AfterEach
	void close() throws Exception {
		harness.close();
	}

	private void sell(KeyedOneInputStreamOperatorTestHarness<String, Sale, Flag> h, String id, String drawing,
			String seller, String buyer, long at) throws Exception {
		h.processElement(new Sale(id, drawing, seller, buyer, 100, at), at);
	}

	private void sell(String id, String drawing, String seller, String buyer, long at) throws Exception {
		sell(harness, id, drawing, seller, buyer, at);
	}

	private List<Flag> flags() {
		return harness.extractOutputValues();
	}

	@Test
	void aRoundTripIsFlaggedAsR1() throws Exception {
		sell("s1", "d1", "alice", "bob", T0);
		assertTrue(flags().isEmpty(), "one sale is no loop");
		sell("s2", "d1", "bob", "alice", T0 + HOUR);

		assertEquals(1, flags().size());
		WashTradeFlagged f = flags().get(0).toEvent(Instant.EPOCH);
		assertEquals(WashTradeRule.WASH_TRADE_RULE_ROUND_TRIP, f.getRule());
		assertEquals("d1", f.getDrawingId());
		assertEquals(List.of("alice", "bob", "alice"), f.getAccountIdsList());
		assertEquals(List.of("s1", "s2"), f.getSaleIdsList());
		assertEquals(T0 / 1000, f.getFirstSaleAt().getSeconds());
		assertEquals((T0 + HOUR) / 1000, f.getLastSaleAt().getSeconds());
		assertEquals(200, f.getVolume());
		assertEquals("WashTradeFlagged", f.getMeta().getEventType());
		assertEquals(f.getFlagId(), f.getMeta().getEventId());
	}

	@Test
	void aCycleOfThreeIsFlaggedAsR3() throws Exception {
		sell("s1", "d1", "alice", "bob", T0);
		sell("s2", "d1", "bob", "carol", T0 + HOUR);
		assertTrue(flags().isEmpty(), "a chain that hasn't come back is no loop");
		sell("s3", "d1", "carol", "alice", T0 + 2 * HOUR);

		assertEquals(1, flags().size());
		Flag f = flags().get(0);
		assertEquals(WashTradeRule.WASH_TRADE_RULE_CYCLE, f.rule());
		assertEquals(List.of("alice", "bob", "carol", "alice"), f.accountIds);
		assertEquals(List.of("s1", "s2", "s3"), f.saleIds);
	}

	@Test
	void onlyTheShortestLoopIsFlagged() throws Exception {
		// alice -> bob -> carol -> bob: bob's round trip closes, not a loop back to alice.
		sell("s1", "d1", "alice", "bob", T0);
		sell("s2", "d1", "bob", "carol", T0 + HOUR);
		sell("s3", "d1", "carol", "bob", T0 + 2 * HOUR);

		assertEquals(1, flags().size());
		assertEquals(List.of("bob", "carol", "bob"), flags().get(0).accountIds);
	}

	@Test
	void theLoopMustCloseWithinSevenDays() throws Exception {
		sell("s1", "d1", "alice", "bob", T0);
		sell("s2", "d1", "bob", "alice", T0 + WEEK.toMillis() + 1);
		assertTrue(flags().isEmpty(), "a week and a millisecond is too long");

		sell("s3", "d2", "alice", "bob", T0);
		sell("s4", "d2", "bob", "alice", T0 + WEEK.toMillis());
		assertEquals(1, flags().size(), "exactly a week still counts");
	}

	@Test
	void drawingsAreKeptApart() throws Exception {
		sell("s1", "d1", "alice", "bob", T0);
		sell("s2", "d2", "bob", "alice", T0 + HOUR);
		assertTrue(flags().isEmpty(), "bob sold alice a different drawing");
	}

	@Test
	void aRepeatedSaleChangesNothing() throws Exception {
		sell("s1", "d1", "alice", "bob", T0);
		sell("s2", "d1", "bob", "alice", T0 + HOUR);
		sell("s2", "d1", "bob", "alice", T0 + HOUR); // the outbox delivered it twice
		assertEquals(1, flags().size());
	}

	@Test
	void timersClearSalesOlderThanTheWindow() throws Exception {
		sell("s1", "d1", "alice", "bob", T0);
		assertEquals(1, harness.numEventTimeTimers());
		harness.processWatermark(T0 + WEEK.toMillis());
		assertEquals(1, harness.numKeyedStateEntries(), "still inside the window");
		harness.processWatermark(T0 + WEEK.toMillis() + 1);
		assertEquals(0, harness.numKeyedStateEntries(), "the drawing's state is gone");
	}

	@Test
	void theHistorySurvivesACheckpointAndRestore() throws Exception {
		sell("s1", "d1", "alice", "bob", T0);
		OperatorSubtaskState snapshot = harness.snapshot(1, T0 + 1);

		// A new operator, as after a restart from the checkpoint (ADR 0018).
		try (KeyedOneInputStreamOperatorTestHarness<String, Sale, Flag> restored = newHarness()) {
			restored.initializeState(snapshot);
			restored.open();
			sell(restored, "s2", "d1", "bob", "alice", T0 + HOUR);
			List<Flag> out = restored.extractOutputValues();
			assertEquals(1, out.size(), "the sale from before the restart closes the loop");
			assertEquals(List.of("s1", "s2"), out.get(0).saleIds);
		}
	}

	@Test
	void theFlagIdDependsOnTheLoopOnly() throws Exception {
		sell("s1", "d1", "alice", "bob", T0);
		sell("s2", "d1", "bob", "alice", T0 + HOUR);
		sell("s3", "d2", "alice", "bob", T0);
		sell("s4", "d2", "bob", "alice", T0 + HOUR);
		Flag a = flags().get(0);
		Flag b = flags().get(1);
		assertEquals(a.flagId(), a.toEvent(Instant.now()).getFlagId(), "the id doesn't depend on when it is built");
		assertNotEquals(a.flagId(), b.flagId(), "another loop, another id");
	}
}
