package com.doodlearena.marketplace.listing;

import java.sql.ResultSet;
import java.sql.SQLException;
import java.time.Instant;
import java.time.OffsetDateTime;
import java.util.UUID;

/** A listing as the API shows it (design doc, section 6.3). */
public record Listing(UUID listingId, UUID drawingId, String sellerId, long price, String status, String buyerId,
		UUID saleId, Instant createdAt, Instant closedAt) {

	static final String COLUMNS = "listing_id, drawing_id, seller_id, price, status, buyer_id, sale_id, created_at, closed_at";

	static Listing map(ResultSet rs, int row) throws SQLException {
		OffsetDateTime closed = rs.getObject("closed_at", OffsetDateTime.class);
		return new Listing(rs.getObject("listing_id", UUID.class), rs.getObject("drawing_id", UUID.class),
				rs.getString("seller_id"), rs.getLong("price"), rs.getString("status"), rs.getString("buyer_id"),
				rs.getObject("sale_id", UUID.class), rs.getObject("created_at", OffsetDateTime.class).toInstant(),
				(closed != null) ? closed.toInstant() : null);
	}

}
