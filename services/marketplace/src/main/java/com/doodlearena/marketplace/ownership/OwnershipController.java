package com.doodlearena.marketplace.ownership;

import java.time.Instant;
import java.time.OffsetDateTime;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.RestController;

import com.doodlearena.marketplace.api.ApiException;

/**
 * A drawing's owner and provenance (design doc, section 6.3). Minted drawings were approved, and
 * approved drawings are public, so any signed-in player may read any history.
 */
@RestController
public class OwnershipController {

	private final JdbcClient db;

	public OwnershipController(JdbcClient db) {
		this.db = db;
	}

	@GetMapping("/api/market/drawings/{id}/ownership-history")
	public OwnershipHistory history(@PathVariable String id) {
		UUID drawingId = parse(id);
		Map<String, Object> ownership = this.db.sql("SELECT owner_id, version FROM ownership WHERE drawing_id = ?")
			.param(drawingId)
			.query()
			.listOfRows()
			.stream()
			.findFirst()
			.orElseThrow(OwnershipController::notFound);
		List<String> authors = this.db.sql("SELECT user_id FROM drawing_authors WHERE drawing_id = ? ORDER BY author_order")
			.param(drawingId)
			.query(String.class)
			.list();
		List<HistoryEvent> events = this.db.sql("""
				SELECT kind, from_owner, to_owner, sale_id, created_at
				FROM ownership_events WHERE drawing_id = ? ORDER BY event_id""")
			.param(drawingId)
			.query((rs, row) -> new HistoryEvent(rs.getString("kind"), rs.getString("from_owner"), rs.getString("to_owner"),
					rs.getObject("sale_id", UUID.class), rs.getObject("created_at", OffsetDateTime.class).toInstant()))
			.list();
		return new OwnershipHistory(drawingId, (String) ownership.get("owner_id"), (Long) ownership.get("version"), authors,
				events);
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

	/** One change of hands, oldest first: MINT (from nobody) or SALE. */
	public record HistoryEvent(String kind, String fromOwner, String toOwner, UUID saleId, Instant createdAt) {
	}

}
