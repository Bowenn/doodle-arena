package com.doodlearena.marketplace.listing;

import java.time.Instant;
import java.util.List;
import java.util.UUID;

import com.doodlearena.marketplace.listing.Royalties.RoyaltyShare;

/**
 * A completed purchase, as {@code POST /listings/{id}/purchase} answers it. The seller received
 * {@code price} minus the royalties.
 */
public record Sale(UUID saleId, UUID listingId, UUID drawingId, String sellerId, String buyerId, long price,
		List<RoyaltyShare> royalties, Instant completedAt) {
}
