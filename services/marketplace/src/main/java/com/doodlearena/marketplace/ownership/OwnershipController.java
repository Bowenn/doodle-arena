package com.doodlearena.marketplace.ownership;

import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.time.OffsetDateTime;
import java.time.ZoneOffset;
import java.util.Base64;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import org.springframework.http.HttpStatus;
import org.springframework.security.core.annotation.AuthenticationPrincipal;
import org.springframework.security.oauth2.jwt.Jwt;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.db.ReadRouting;

/**
 * A drawing's owner and provenance (design doc, section 6.3). Minted drawings were approved, and
 * approved drawings are public, so any signed-in player may read any history.
 */
@RestController
public class OwnershipController {

	private static final int DEFAULT_LIMIT = 20;

	private static final int MAX_LIMIT = 50;

	private final ReadRouting reads;

	public OwnershipController(ReadRouting reads) {
		this.reads = reads;
	}

	@GetMapping("/api/market/drawings/{id}/ownership-history")
	public OwnershipHistory history(@PathVariable String id) {
		UUID drawingId = parse(id);
		// The replica, unless it hasn't replayed the caller's last write yet (ADR 0020).
		return this.reads.read((db) -> {
		Map<String, Object> ownership = db.sql("SELECT owner_id, version FROM ownership WHERE drawing_id = ?")
			.param(drawingId)
			.query()
			.listOfRows()
			.stream()
			.findFirst()
			.orElseThrow(OwnershipController::notFound);
		List<String> authors = db.sql("SELECT user_id FROM drawing_authors WHERE drawing_id = ? ORDER BY author_order")
			.param(drawingId)
			.query(String.class)
			.list();
		List<HistoryEvent> events = db.sql("""
				SELECT e.kind, e.from_owner, e.to_owner, e.sale_id, l.price, e.created_at
				FROM ownership_events e LEFT JOIN listings l ON l.sale_id = e.sale_id
				WHERE e.drawing_id = ? ORDER BY e.event_id""")
			.param(drawingId)
			.query((rs, row) -> new HistoryEvent(rs.getString("kind"), rs.getString("from_owner"), rs.getString("to_owner"),
					rs.getObject("sale_id", UUID.class), rs.getObject("price", Long.class),
					rs.getObject("created_at", OffsetDateTime.class).toInstant()))
			.list();
		return new OwnershipHistory(drawingId, (String) ownership.get("owner_id"), (Long) ownership.get("version"), authors,
				events);
		});
	}

	/**
	 * The caller's drawings, most recently acquired first, each with its active listing if any
	 * ("my drawings" in the web app). Only {@code owner=me}: who owns what is visible one drawing at
	 * a time through the history, not as a per-user list.
	 */
	@GetMapping("/api/market/drawings")
	public OwnedPage owned(@AuthenticationPrincipal Jwt token, @RequestParam(required = false) String owner,
			@RequestParam(defaultValue = "" + DEFAULT_LIMIT) int limit, @RequestParam(required = false) String cursor) {
		if (!"me".equals(owner)) {
			throw ApiException.invalidRequest("owner must be me");
		}
		if (limit < 1 || limit > MAX_LIMIT) {
			throw ApiException.invalidRequest("limit must be between 1 and " + MAX_LIMIT);
		}
		Cursor after = (cursor == null || cursor.isEmpty()) ? null : Cursor.decode(cursor);
		// The replica, unless it hasn't replayed the caller's last purchase yet (ADR 0020).
		List<OwnedDrawing> rows = this.reads.read((db) -> db.sql("""
				SELECT o.drawing_id, o.version, o.minted_at, o.updated_at, l.listing_id, l.price, l.created_at AS listed_at
				FROM ownership o
				LEFT JOIN listings l ON l.drawing_id = o.drawing_id AND l.status = 'ACTIVE'
				WHERE o.owner_id = :owner
					AND (CAST(:t AS TIMESTAMPTZ) IS NULL OR (o.updated_at, o.drawing_id) < (:t, :id))
				ORDER BY o.updated_at DESC, o.drawing_id DESC
				LIMIT :limit""")
			.param("owner", token.getSubject())
			.param("t", after == null ? null : after.updatedAt().atOffset(ZoneOffset.UTC))
			.param("id", after == null ? null : after.drawingId())
			.param("limit", limit + 1)
			.query((rs, row) -> {
				UUID listingId = rs.getObject("listing_id", UUID.class);
				OwnedListing listing = (listingId == null) ? null
						: new OwnedListing(listingId, rs.getLong("price"),
								rs.getObject("listed_at", OffsetDateTime.class).toInstant());
				return new OwnedDrawing(rs.getObject("drawing_id", UUID.class), rs.getLong("version"),
						rs.getObject("minted_at", OffsetDateTime.class).toInstant(),
						rs.getObject("updated_at", OffsetDateTime.class).toInstant(), listing);
			})
			.list());
		List<OwnedDrawing> page = rows.subList(0, Math.min(limit, rows.size()));
		String next = (rows.size() > limit)
				? new Cursor(page.getLast().updatedAt(), page.getLast().drawingId()).encode() : null;
		return new OwnedPage(page, next);
	}

	public record OwnedPage(List<OwnedDrawing> drawings, String nextCursor) {
	}

	public record OwnedDrawing(UUID drawingId, long version, Instant mintedAt, Instant updatedAt,
			OwnedListing listing) {
	}

	public record OwnedListing(UUID listingId, long price, Instant createdAt) {
	}

	/** Keyset position: the last row's (updated_at, drawing_id), opaque to clients. */
	record Cursor(Instant updatedAt, UUID drawingId) {

		String encode() {
			String raw = this.updatedAt + "|" + this.drawingId;
			return Base64.getUrlEncoder().withoutPadding().encodeToString(raw.getBytes(StandardCharsets.UTF_8));
		}

		static Cursor decode(String cursor) {
			try {
				String[] parts = new String(Base64.getUrlDecoder().decode(cursor), StandardCharsets.UTF_8).split("\\|", 2);
				return new Cursor(Instant.parse(parts[0]), UUID.fromString(parts[1]));
			}
			catch (RuntimeException ex) {
				throw ApiException.invalidRequest("invalid cursor");
			}
		}

	}

	/** A malformed id gets the same answer as an unknown one: ids leak nothing. */
	private static UUID parse(String id) {
		try {
			return UUID.fromString(id);
		}
		catch (IllegalArgumentException ex) {
			throw notFound();
		}
	}

	private static ApiException notFound() {
		return new ApiException(HttpStatus.NOT_FOUND, "DRAWING_NOT_FOUND", "no such drawing");
	}

	public record OwnershipHistory(UUID drawingId, String ownerId, long version, List<String> authorIds,
			List<HistoryEvent> events) {
	}

	/** One change of hands, oldest first: MINT (from nobody, no price) or SALE. */
	public record HistoryEvent(String kind, String fromOwner, String toOwner, UUID saleId, Long price,
			Instant createdAt) {
	}

}
