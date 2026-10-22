package com.doodlearena.analytics.wash;

import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.WashTradeFlagged;
import com.doodlearena.proto.doodle.events.v1.WashTradeRule;
import com.google.protobuf.Timestamp;
import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.UUID;

/**
 * One loop of sales inside the job. A Flink POJO rather than the protobuf message, like
 * WindowTop: the message is built only at the Kafka sink (toEvent).
 */
public class Flag {
	/** True for R1 (two sales, A to B to A), false for R3 (three or more). */
	public boolean roundTrip;
	public String drawingId;
	/** The accounts in the order the drawing passed through them, the first repeated at the end. */
	public List<String> accountIds = new ArrayList<>();
	/** The loop's sales in the order they happened. */
	public List<String> saleIds = new ArrayList<>();
	public long firstSaleAtMs;
	public long lastSaleAtMs;
	/** Coins paid across the loop's sales. */
	public long volume;

	public Flag() {}

	public WashTradeRule rule() {
		return roundTrip ? WashTradeRule.WASH_TRADE_RULE_ROUND_TRIP : WashTradeRule.WASH_TRADE_RULE_CYCLE;
	}

	/** The metric's rule attribute. */
	public String ruleName() {
		return roundTrip ? "round_trip" : "cycle";
	}

	/**
	 * From the rule and the sales: the same loop gets the same id when it is flagged again after a
	 * restart or delivered twice (at-least-once, ADR 0018), so a consumer can drop the repeat.
	 */
	public String flagId() {
		return UUID.nameUUIDFromBytes((ruleName() + "/" + String.join(",", saleIds)).getBytes(StandardCharsets.UTF_8))
				.toString();
	}

	/** The analytics.flags.v1 message. */
	public WashTradeFlagged toEvent(Instant now) {
		String id = flagId();
		return WashTradeFlagged.newBuilder()
				.setMeta(EventMeta.newBuilder()
						.setEventId(id)
						.setEventType("WashTradeFlagged")
						.setSchemaVersion(1)
						.setOccurredAt(ts(now.toEpochMilli()))
						.setProducer("analytics"))
				.setFlagId(id)
				.setRule(rule())
				.setDrawingId(drawingId)
				.addAllAccountIds(accountIds)
				.addAllSaleIds(saleIds)
				.setFirstSaleAt(ts(firstSaleAtMs))
				.setLastSaleAt(ts(lastSaleAtMs))
				.setVolume(volume)
				.build();
	}

	private static Timestamp ts(long ms) {
		return Timestamp.newBuilder().setSeconds(Math.floorDiv(ms, 1000)).setNanos(Math.floorMod(ms, 1000) * 1_000_000).build();
	}
}
