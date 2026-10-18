package com.doodlearena.marketplace.listing;

import java.time.Instant;
import java.time.OffsetDateTime;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Isolation;
import org.springframework.transaction.annotation.Transactional;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.InsufficientFundsException;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.PostResult;
import com.doodlearena.marketplace.ledger.Posting;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.listing.Royalties.RoyaltyShare;
import com.doodlearena.marketplace.outbox.OutboxWriter;
import com.doodlearena.marketplace.telemetry.MarketMetrics;
import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.RoyaltyPayment;
import com.doodlearena.proto.doodle.events.v1.SaleCompleted;
import com.doodlearena.proto.doodle.events.v1.SaleEvent;

/**
 * Buys a listing: coins and ownership move in one Postgres transaction (design doc, section 8).
 *
 * <p>Locks are always taken in the same order, listing, then ownership, then balances in ascending
 * account order (inside {@link LedgerService#post}), so purchases can't deadlock each other. The
 * listing's {@code FOR UPDATE} is where concurrent buyers queue: the first one sells it, and every
 * other one then finds it SOLD and gets 409 without moving a coin.
 *
 * <p>The price is split between the seller and the drawing's other authors ({@link Royalties}).
 */
@Service
public class PurchaseService {

	private static final Logger log = LoggerFactory.getLogger(PurchaseService.class);

	private final JdbcClient db;

	private final LedgerService ledger;

	private final OutboxWriter outbox;

	private final MarketMetrics metrics;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	private final int royaltyPercent;

	public PurchaseService(JdbcClient db, LedgerService ledger, OutboxWriter outbox, MarketMetrics metrics,
			@Value("${app.royalties.percent}") int royaltyPercent) {
		this.db = db;
		this.ledger = ledger;
		this.outbox = outbox;
		this.metrics = metrics;
		this.royaltyPercent = royaltyPercent;
	}

	/** Runs inside the idempotency work's transaction; any exception rolls the whole purchase back. */
	@Transactional(isolation = Isolation.READ_COMMITTED)
	@SuppressWarnings("try") // the scope only has to be open while the statements run
	public Sale purchase(String buyerId, UUID listingId) {
		Span span = this.tracer.spanBuilder("market.purchase")
			.setAttribute("listing.id", listingId.toString())
			.setAttribute("user.id", buyerId)
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			Sale sale = buy(buyerId, listingId, span);
			// Counted once the sale is real: the idempotency completion or the commit can still
			// roll it back.
			TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
				@Override
				public void afterCommit() {
					PurchaseService.this.metrics.purchase("success");
				}
			});
			return sale;
		}
		catch (ApiException ex) {
			span.setAttribute("purchase.result", ex.code());
			throw ex;
		}
		catch (RuntimeException ex) {
			span.recordException(ex);
			span.setStatus(StatusCode.ERROR);
			throw ex;
		}
		finally {
			span.end();
		}
	}

	private Sale buy(String buyerId, UUID listingId, Span span) {
		// 1. The listing: concurrent buyers wait here, one at a time.
		long started = System.nanoTime();
		Listing listing = this.db.sql("SELECT " + Listing.COLUMNS + " FROM listings WHERE listing_id = ? FOR UPDATE")
			.param(listingId)
			.query(Listing::map)
			.optional()
			.orElseThrow(ListingService::notFound);
		this.metrics.recordLockWait("purchase", System.nanoTime() - started);
		span.setAttribute("drawing.id", listing.drawingId().toString());
		if (!listing.status().equals("ACTIVE")) {
			this.metrics.purchase("conflict");
			throw ListingService.notAvailable();
		}
		if (listing.sellerId().equals(buyerId)) {
			throw new ApiException(HttpStatus.UNPROCESSABLE_CONTENT, "CANNOT_BUY_OWN_LISTING", "this is your own listing");
		}

		// 2. The drawing's ownership row: the seller must still own it.
		Map<String, Object> ownership = this.db.sql("SELECT owner_id, version FROM ownership WHERE drawing_id = ? FOR UPDATE")
			.param(listing.drawingId())
			.query()
			.singleRow();
		long version = (Long) ownership.get("version");
		if (!listing.sellerId().equals(ownership.get("owner_id"))) {
			// An active listing whose seller isn't the owner is a broken invariant, not the buyer's problem.
			throw new IllegalStateException("listing " + listingId + " is active but its seller no longer owns the drawing");
		}

		// 3. The coins: the buyer pays the price, the seller gets it minus the royalties, and the other
		// authors share those. Balances are locked in account order; a listing can be paid for only once.
		List<String> authors = this.db.sql("SELECT user_id FROM drawing_authors WHERE drawing_id = ? ORDER BY author_order")
			.param(listing.drawingId())
			.query(String.class)
			.list();
		List<RoyaltyShare> royalties = Royalties.split(listing.price(), authors, listing.sellerId(), buyerId,
				this.royaltyPercent);
		long royaltyTotal = Royalties.total(royalties);
		List<Posting> postings = new ArrayList<>();
		postings.add(Posting.debit(AccountRef.user(buyerId), listing.price()));
		postings.add(Posting.credit(AccountRef.user(listing.sellerId()), listing.price() - royaltyTotal));
		for (RoyaltyShare royalty : royalties) {
			postings.add(Posting.credit(AccountRef.user(royalty.userId()), royalty.amount()));
		}
		span.setAttribute("sale.royalty_total", royaltyTotal);
		PostResult payment;
		try {
			payment = this.ledger.post(TransactionType.SALE, "sale:" + listingId,
					Map.of("listing_id", listingId.toString(), "drawing_id", listing.drawingId().toString()), postings);
		}
		catch (InsufficientFundsException ex) {
			this.metrics.purchase("insufficient_funds");
			throw new ApiException(HttpStatus.UNPROCESSABLE_CONTENT, "INSUFFICIENT_FUNDS",
					"the price is " + listing.price() + " coins");
		}
		if (payment.replayed()) {
			throw new IllegalStateException("listing " + listingId + " was already paid for");
		}
		UUID saleId = payment.txId();

		// 4. The listing closes and the drawing changes hands, with a provenance event.
		Instant completedAt = this.db.sql("""
				UPDATE listings SET status = 'SOLD', buyer_id = ?, sale_id = ?, closed_at = now()
				WHERE listing_id = ? RETURNING closed_at""")
			.params(buyerId, saleId, listingId)
			.query(OffsetDateTime.class)
			.single()
			.toInstant();
		int moved = this.db.sql("""
				UPDATE ownership SET owner_id = ?, version = version + 1, updated_at = now()
				WHERE drawing_id = ? AND version = ?""")
			.params(buyerId, listing.drawingId(), version)
			.update();
		if (moved != 1) {
			throw new IllegalStateException("ownership of " + listing.drawingId() + " changed under its row lock");
		}
		this.db.sql("""
				INSERT INTO ownership_events (drawing_id, kind, from_owner, to_owner, sale_id, source_event_id)
				VALUES (?, 'SALE', ?, ?, ?, ?)""")
			.params(listing.drawingId(), listing.sellerId(), buyerId, saleId, listingId.toString())
			.update();

		// 5. The event, committed with everything else.
		EventMeta meta = OutboxWriter.meta("SaleCompleted", completedAt);
		this.outbox.write(OutboxWriter.SALES_TOPIC, listing.drawingId().toString(), meta, SaleEvent.newBuilder()
			.setSaleCompleted(SaleCompleted.newBuilder()
				.setMeta(meta)
				.setSaleId(saleId.toString())
				.setListingId(listingId.toString())
				.setDrawingId(listing.drawingId().toString())
				.setSellerId(listing.sellerId())
				.setBuyerId(buyerId)
				.setPrice(listing.price())
				.addAllRoyalties(royalties.stream()
					.map((r) -> RoyaltyPayment.newBuilder().setUserId(r.userId()).setAmount(r.amount()).build())
					.toList())
				.setCompletedAt(OutboxWriter.timestamp(completedAt)))
			.build()
			.toByteArray());

		span.setAttribute("sale.id", saleId.toString());
		log.atInfo()
			.addKeyValue("listing_id", listingId)
			.addKeyValue("drawing_id", listing.drawingId())
			.addKeyValue("user_id", buyerId)
			.addKeyValue("seller_id", listing.sellerId())
			.addKeyValue("sale_id", saleId)
			.addKeyValue("price", listing.price())
			.addKeyValue("royalties", royalties.size())
			.addKeyValue("royalty_total", royaltyTotal)
			.log("purchase completed");
		return new Sale(saleId, listingId, listing.drawingId(), listing.sellerId(), buyerId, listing.price(), royalties,
				completedAt);
	}

}
