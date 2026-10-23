package com.doodlearena.marketplace.drop;

import java.time.Instant;
import java.util.UUID;

/**
 * A drop as the API shows it. {@code status} is UPCOMING, OPEN, SOLD_OUT or CLOSED;
 * {@code editions_left} counts editions neither sold nor reserved.
 */
public record DropView(UUID dropId, UUID drawingId, int editions, long editionsLeft, long price, Instant opensAt,
		Instant closesAt, String status) {
}
