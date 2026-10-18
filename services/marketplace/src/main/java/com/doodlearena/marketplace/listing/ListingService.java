package com.doodlearena.marketplace.listing;

import java.util.List;
import java.util.UUID;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.outbox.OutboxWriter;
import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.ListingCancelled;
import com.doodlearena.proto.doodle.events.v1.ListingCreated;
import com.doodlearena.proto.doodle.events.v1.ListingEvent;

/**
 * Listing and withdrawing drawings (design doc, section 7.8). Only the owner lists a drawing, and
 * a drawing has at most one active listing. Each change writes its event to the outbox in the
 * same transaction.
 */
@Service
public class ListingService {

	public static final long MAX_PRICE = 1_000_000;

	private static final Logger log = LoggerFactory.getLogger(ListingService.class);

	private final JdbcClient db;

	private final OutboxWriter outbox;

	public ListingService(JdbcClient db, OutboxWriter outbox) {
		this.db = db;
		this.outbox = outbox;
	}

	/** Runs inside the idempotency work's transaction ({@code POST /listings} needs a key). */
	@Transactional
	public Listing create(String sellerId, UUID drawingId, long price) {
		// FOR SHARE: a concurrent sale of this drawing (which locks the row FOR UPDATE) can't move it
		// to another owner between this check and the insert.
		String owner = this.db.sql("SELECT owner_id FROM ownership WHERE drawing_id = ? FOR SHARE")
			.param(drawingId)
			.query(String.class)
			.optional()
			.orElseThrow(() -> new ApiException(HttpStatus.NOT_FOUND, "DRAWING_NOT_FOUND", "no such drawing"));
		if (!owner.equals(sellerId)) {
			throw new ApiException(HttpStatus.FORBIDDEN, "NOT_OWNER", "only the drawing's owner can list it");
		}
		// The partial unique index decides between two concurrent listings of one drawing.
		Listing listing = this.db.sql("INSERT INTO listings (drawing_id, seller_id, price, status) VALUES (?, ?, ?, 'ACTIVE') "
				+ "ON CONFLICT (drawing_id) WHERE status = 'ACTIVE' DO NOTHING RETURNING " + Listing.COLUMNS)
			.params(drawingId, sellerId, price)
			.query(Listing::map)
			.optional()
			.orElseThrow(() -> new ApiException(HttpStatus.CONFLICT, "ALREADY_LISTED", "this drawing is already listed"));
		EventMeta meta = OutboxWriter.meta("ListingCreated", listing.createdAt());
		this.outbox.write(OutboxWriter.LISTINGS_TOPIC, drawingId.toString(), meta, ListingEvent.newBuilder()
			.setListingCreated(ListingCreated.newBuilder()
				.setMeta(meta)
				.setListingId(listing.listingId().toString())
				.setDrawingId(drawingId.toString())
				.setSellerId(sellerId)
				.setPrice(price)
				.setCreatedAt(OutboxWriter.timestamp(listing.createdAt())))
			.build()
			.toByteArray());
		log.atInfo()
			.addKeyValue("listing_id", listing.listingId())
			.addKeyValue("drawing_id", drawingId)
			.addKeyValue("user_id", sellerId)
			.addKeyValue("price", price)
			.log("listing created");
		return listing;
	}

	/** Withdraws a listing. Cancelling a cancelled listing answers the same again. */
	@Transactional
	public Listing cancel(String userId, UUID listingId) {
		Listing listing = this.db.sql("SELECT " + Listing.COLUMNS + " FROM listings WHERE listing_id = ? FOR UPDATE")
			.param(listingId)
			.query(Listing::map)
			.optional()
			.orElseThrow(ListingService::notFound);
		if (!listing.sellerId().equals(userId)) {
			throw new ApiException(HttpStatus.FORBIDDEN, "NOT_OWNER", "only the seller can cancel a listing");
		}
		switch (listing.status()) {
			case "CANCELLED" -> {
				return listing;
			}
			case "SOLD" -> throw notAvailable();
			default -> {
			}
		}
		Listing cancelled = this.db.sql("UPDATE listings SET status = 'CANCELLED', closed_at = now() WHERE listing_id = ? "
				+ "RETURNING " + Listing.COLUMNS)
			.param(listingId)
			.query(Listing::map)
			.single();
		EventMeta meta = OutboxWriter.meta("ListingCancelled", cancelled.closedAt());
		this.outbox.write(OutboxWriter.LISTINGS_TOPIC, listing.drawingId().toString(), meta, ListingEvent.newBuilder()
			.setListingCancelled(ListingCancelled.newBuilder()
				.setMeta(meta)
				.setListingId(listingId.toString())
				.setDrawingId(listing.drawingId().toString())
				.setSellerId(userId)
				.setCancelledAt(OutboxWriter.timestamp(cancelled.closedAt())))
			.build()
			.toByteArray());
		log.atInfo().addKeyValue("listing_id", listingId).addKeyValue("user_id", userId).log("listing cancelled");
		return cancelled;
	}

	public Listing get(UUID listingId) {
		return this.db.sql("SELECT " + Listing.COLUMNS + " FROM listings WHERE listing_id = ?")
			.param(listingId)
			.query(Listing::map)
			.optional()
			.orElseThrow(ListingService::notFound);
	}

	/** Active listings, newest first, below {@code before} when set. */
	public List<Listing> active(UUID drawingId, String sellerId, UUID before, int limit) {
		return this.db.sql("SELECT " + Listing.COLUMNS + " FROM listings WHERE status = 'ACTIVE'"
				+ " AND (CAST(:drawing AS UUID) IS NULL OR drawing_id = :drawing)"
				+ " AND (CAST(:seller AS TEXT) IS NULL OR seller_id = :seller)"
				+ " AND (CAST(:before AS UUID) IS NULL OR listing_id < :before)"
				+ " ORDER BY listing_id DESC LIMIT :limit")
			.param("drawing", drawingId)
			.param("seller", sellerId)
			.param("before", before)
			.param("limit", limit)
			.query(Listing::map)
			.list();
	}

	public static ApiException notFound() {
		return new ApiException(HttpStatus.NOT_FOUND, "LISTING_NOT_FOUND", "no such listing");
	}

	public static ApiException notAvailable() {
		return new ApiException(HttpStatus.CONFLICT, "LISTING_NOT_AVAILABLE", "this listing is sold or cancelled");
	}

}
