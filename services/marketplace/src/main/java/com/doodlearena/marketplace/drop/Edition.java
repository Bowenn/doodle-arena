package com.doodlearena.marketplace.drop;

import java.time.Instant;
import java.util.UUID;

/** One numbered edition of a drop, owned by its buyer (ADR 0022). */
public record Edition(UUID dropId, UUID drawingId, int editionNo, int editions, long price, Instant purchasedAt) {
}
