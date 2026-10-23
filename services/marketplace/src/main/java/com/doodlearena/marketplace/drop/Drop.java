package com.doodlearena.marketplace.drop;

import java.time.Instant;
import java.util.UUID;

/** A limited drop as stored (ADR 0022). Its terms never change after it was created. */
public record Drop(UUID dropId, UUID drawingId, int editions, long price, Instant opensAt, Instant closesAt,
		int admitPerSecond, Instant createdAt) {

	static final String COLUMNS = "drop_id, drawing_id, editions, price, opens_at, closes_at, admit_per_second, created_at";

	/** UPCOMING, OPEN or CLOSED, by the time alone (SOLD_OUT comes from the inventory). */
	String phase(Instant now) {
		if (now.isBefore(this.opensAt)) {
			return "UPCOMING";
		}
		return now.isBefore(this.closesAt) ? "OPEN" : "CLOSED";
	}

}
