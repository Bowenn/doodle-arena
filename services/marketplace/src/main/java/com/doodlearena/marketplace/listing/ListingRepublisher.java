package com.doodlearena.marketplace.listing;

import java.time.Instant;
import java.util.List;
import java.util.UUID;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

import com.doodlearena.marketplace.outbox.OutboxWriter;
import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.ListingCreated;
import com.doodlearena.proto.doodle.events.v1.ListingEvent;

/**
 * Announces every active listing again (ADR 0023): one ListingCreated per ACTIVE listing, with
 * its own id, seller, price and creation time, through the outbox like any other. Search's
 * indexer treats them as it treats the originals (a listing it knows is a no-op), so this fills
 * its projection the first time, and after a restore from backup, when the originals are older
 * than the topics' 7 days. It moves no money and changes no listing.
 */
@Service
public class ListingRepublisher {

	private static final Logger log = LoggerFactory.getLogger(ListingRepublisher.class);

	private final JdbcClient db;

	private final OutboxWriter outbox;

	public ListingRepublisher(JdbcClient db, OutboxWriter outbox) {
		this.db = db;
		this.outbox = outbox;
	}

	/** The result: how many listings were announced. */
	public record Republished(int listings) {
	}

	@Transactional
	public Republished republish(String adminId) {
		record Active(UUID listingId, UUID drawingId, String sellerId, long price, Instant createdAt) {
		}
		List<Active> active = this.db
			.sql("SELECT listing_id, drawing_id, seller_id, price, created_at FROM listings WHERE status = 'ACTIVE' "
					+ "ORDER BY listing_id")
			.query(Active.class)
			.list();
		// The event happens now (its meta time, which the index lag measures from); the listing's
		// own creation time travels in created_at.
		Instant now = Instant.now();
		for (Active listing : active) {
			EventMeta meta = OutboxWriter.meta("ListingCreated", now);
			this.outbox.write(OutboxWriter.LISTINGS_TOPIC, listing.drawingId().toString(), meta, ListingEvent.newBuilder()
				.setListingCreated(ListingCreated.newBuilder()
					.setMeta(meta)
					.setListingId(listing.listingId().toString())
					.setDrawingId(listing.drawingId().toString())
					.setSellerId(listing.sellerId())
					.setPrice(listing.price())
					.setCreatedAt(OutboxWriter.timestamp(listing.createdAt())))
				.build()
				.toByteArray());
		}
		log.atInfo().addKeyValue("user_id", adminId).addKeyValue("listings", active.size()).log("active listings republished");
		return new Republished(active.size());
	}

}
