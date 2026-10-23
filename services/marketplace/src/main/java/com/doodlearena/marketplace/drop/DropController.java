package com.doodlearena.marketplace.drop;

import java.time.Instant;
import java.time.format.DateTimeParseException;
import java.util.List;
import java.util.UUID;

import jakarta.servlet.http.HttpServletRequest;
import tools.jackson.core.JacksonException;
import tools.jackson.databind.JsonNode;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.security.core.annotation.AuthenticationPrincipal;
import org.springframework.security.oauth2.jwt.Jwt;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestHeader;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.api.ErrorBody;
import com.doodlearena.marketplace.idempotency.IdempotencyService;

/**
 * Limited drops (design doc, section 6.3; ADR 0022). Creating one needs the admin role
 * (SecurityConfig); everything else any signed-in player may do, as the token's subject.
 */
@RestController
@RequestMapping("/api/market")
public class DropController {

	private static final int MAX_LIMIT = 50;

	private final DropService drops;

	private final IdempotencyService idempotency;

	private final JsonMapper json;

	public DropController(DropService drops, IdempotencyService idempotency, JsonMapper json) {
		this.drops = drops;
		this.idempotency = idempotency;
		this.json = json;
	}

	@PostMapping("/drops")
	public ResponseEntity<String> create(@AuthenticationPrincipal Jwt token,
			@RequestHeader(name = "Idempotency-Key", required = false) String key,
			@RequestBody(required = false) String body, HttpServletRequest request) {
		CreateDrop input = parse(body);
		String admin = token.getSubject();
		return this.idempotency
			.execute(admin, key, "create_drop", IdempotencyService.hash("POST", request.getRequestURI(), body),
					ErrorBody.requestId(request), HttpStatus.CREATED,
					(claim) -> this.drops.create(admin, input.drawingId(), input.editions(), input.price(), input.opensAt(),
							input.durationMinutes()))
			.toResponseEntity();
	}

	@GetMapping("/drops")
	public DropsPage list() {
		return new DropsPage(this.drops.list());
	}

	@GetMapping("/drops/{id}")
	public DropView get(@PathVariable String id) {
		return this.drops.get(dropId(id));
	}

	@PostMapping("/drops/{id}/join")
	public QueueStatus join(@AuthenticationPrincipal Jwt token, @PathVariable String id) {
		return this.drops.join(token.getSubject(), dropId(id));
	}

	@GetMapping("/drops/{id}/queue")
	public QueueStatus queue(@AuthenticationPrincipal Jwt token, @PathVariable String id) {
		return this.drops.queue(token.getSubject(), dropId(id));
	}

	@PostMapping("/drops/{id}/purchase")
	public ResponseEntity<String> purchase(@AuthenticationPrincipal Jwt token, @PathVariable String id,
			@RequestHeader(name = "Idempotency-Key", required = false) String key,
			@RequestBody(required = false) String body, HttpServletRequest request) {
		return this.drops
			.purchase(token.getSubject(), dropId(id), key,
					IdempotencyService.hash("POST", request.getRequestURI(), body), ErrorBody.requestId(request))
			.toResponseEntity();
	}

	@GetMapping("/editions")
	public EditionsPage editions(@AuthenticationPrincipal Jwt token,
			@RequestParam(defaultValue = "me") String owner, @RequestParam(defaultValue = "20") int limit) {
		if (!owner.equals("me")) {
			throw ApiException.invalidRequest("only owner=me is supported");
		}
		if (limit < 1 || limit > MAX_LIMIT) {
			throw ApiException.invalidRequest("limit must be between 1 and " + MAX_LIMIT);
		}
		return new EditionsPage(this.drops.editions(token.getSubject(), limit));
	}

	/** A malformed drop id gets the same answer as an unknown one. */
	private static UUID dropId(String id) {
		try {
			return UUID.fromString(id);
		}
		catch (IllegalArgumentException ex) {
			throw new ApiException(HttpStatus.NOT_FOUND, "DROP_NOT_FOUND", "no drop has this id");
		}
	}

	/** Strict like a listing: whole numbers as JSON integers, the time as RFC 3339. */
	private CreateDrop parse(String body) {
		JsonNode root;
		try {
			root = this.json.readTree(body == null ? "" : body);
		}
		catch (JacksonException ex) {
			throw ApiException.invalidRequest(
					"the body must be {\"drawing_id\", \"editions\", \"price\", \"opens_at\", \"duration_minutes\"}");
		}
		JsonNode drawing = (root == null) ? null : root.get("drawing_id");
		JsonNode opens = (root == null) ? null : root.get("opens_at");
		if (drawing == null || !drawing.isString() || opens == null || !opens.isString()) {
			throw ApiException.invalidRequest("drawing_id (a UUID) and opens_at (an RFC 3339 time) are required");
		}
		UUID drawingId;
		Instant opensAt;
		try {
			drawingId = UUID.fromString(drawing.stringValue());
			opensAt = Instant.parse(opens.stringValue());
		}
		catch (IllegalArgumentException | DateTimeParseException ex) {
			throw ApiException.invalidRequest("drawing_id must be a UUID and opens_at an RFC 3339 time");
		}
		int editions = whole(root, "editions", 1, DropService.MAX_EDITIONS);
		long price = whole(root, "price", 1, DropService.MAX_PRICE);
		int duration = whole(root, "duration_minutes", 1, DropService.MAX_DURATION_MINUTES);
		return new CreateDrop(drawingId, editions, price, opensAt, duration);
	}

	private static int whole(JsonNode root, String field, long min, long max) {
		JsonNode node = root.get(field);
		if (node == null || !node.isIntegralNumber() || !node.canConvertToLong() || node.longValue() < min
				|| node.longValue() > max) {
			throw ApiException.invalidRequest(field + " must be a whole number from " + min + " to " + max);
		}
		return (int) node.longValue();
	}

	record CreateDrop(UUID drawingId, int editions, long price, Instant opensAt, int durationMinutes) {
	}

	public record DropsPage(List<DropView> drops) {
	}

	public record EditionsPage(List<Edition> editions) {
	}

}
