package com.doodlearena.marketplace.listing;

import java.time.Instant;
import java.util.UUID;

/** A completed purchase, as {@code POST /listings/{id}/purchase} answers it. */
public record Sale(UUID saleId, UUID listingId, UUID drawingId, String sellerId, String buyerId, long price,
		Instant completedAt) {
}
