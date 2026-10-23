package com.doodlearena.marketplace.drop;

import java.time.Instant;
import java.util.UUID;

/**
 * Where the caller stands in a drop's waiting room. {@code position} is the number of places
 * still ahead of being admitted (0 once admitted), null if they haven't joined; before the open
 * everyone who joined is waiting for the lottery.
 */
public record QueueStatus(UUID dropId, Long position, boolean admitted, long waiting, long editionsLeft,
		Instant opensAt, String status) {
}
