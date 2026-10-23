package com.doodlearena.marketplace.listing;

import java.nio.charset.StandardCharsets;
import java.util.Base64;
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
import org.springframework.web.bind.annotation.DeleteMapping;
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

/** Listings (design doc, section 6.3). The caller is always the token's subject. */
@RestController
@RequestMapping("/api/market/listings")
public class ListingController {

	private static final int DEFAULT_LIMIT = 20;

	private static final int MAX_LIMIT = 50;

	private final ListingService listings;

	private final PurchaseService purchases;

	private final IdempotencyService idempotency;

	private final JsonMapper json;

	private final ListingRepublisher republisher;

	public ListingController(ListingService listings, PurchaseService purchases, IdempotencyService idempotency,
			JsonMapper json, ListingRepublisher republisher) {
		this.listings = listings;
		this.purchases = purchases;
		this.idempotency = idempotency;
		this.json = json;
		this.republisher = republisher;
	}

	/**
	 * Admins only (SecurityConfig): every active listing announced again on market.listings.v1,
	 * for search's projection after its first deploy or a restore (ADR 0023).
	 */
	@PostMapping("/republish")
	public ResponseEntity<String> republish(@AuthenticationPrincipal Jwt token,
			@RequestHeader(name = "Idempotency-Key", required = false) String key,
			@RequestBody(required = false) String body, HttpServletRequest request) {
		String admin = token.getSubject();
		return this.idempotency
			.execute(admin, key, "republish_listings", IdempotencyService.hash("POST", request.getRequestURI(), body),
					ErrorBody.requestId(request), HttpStatus.OK, (claim) -> this.republisher.republish(admin))
			.toResponseEntity();
	}

	@PostMapping
	public ResponseEntity<String> create(@AuthenticationPrincipal Jwt token,
			@RequestHeader(name = "Idempotency-Key", required = false) String key,
			@RequestBody(required = false) String body, HttpServletRequest request) {
		CreateListingRequest input = parse(body);
		String seller = token.getSubject();
		return this.idempotency
			.execute(seller, key, "create_listing", IdempotencyService.hash("POST", request.getRequestURI(), body),
					ErrorBody.requestId(request), HttpStatus.CREATED,
					(claim) -> this.listings.create(seller, input.drawingId(), input.price()))
			.toResponseEntity();
	}

	@PostMapping("/{id}/purchase")
	public ResponseEntity<String> purchase(@AuthenticationPrincipal Jwt token, @PathVariable String id,
			@RequestHeader(name = "Idempotency-Key", required = false) String key,
			@RequestBody(required = false) String body, HttpServletRequest request) {
		UUID listingId = parseId(id);
		String buyer = token.getSubject();
		return this.idempotency
			.execute(buyer, key, "purchase", IdempotencyService.hash("POST", request.getRequestURI(), body),
					ErrorBody.requestId(request), HttpStatus.OK, (claim) -> this.purchases.purchase(buyer, listingId))
			.toResponseEntity();
	}

	@DeleteMapping("/{id}")
	public Listing cancel(@AuthenticationPrincipal Jwt token, @PathVariable String id) {
		return this.listings.cancel(token.getSubject(), parseId(id));
	}

	@GetMapping("/{id}")
	public Listing get(@PathVariable String id) {
		return this.listings.get(parseId(id));
	}

	@GetMapping
	public ListingsPage active(@RequestParam(name = "drawing_id", required = false) String drawingId,
			@RequestParam(name = "seller_id", required = false) String sellerId,
			@RequestParam(defaultValue = "" + DEFAULT_LIMIT) int limit, @RequestParam(required = false) String cursor) {
		if (limit < 1 || limit > MAX_LIMIT) {
			throw ApiException.invalidRequest("limit must be between 1 and " + MAX_LIMIT);
		}
		UUID drawing = (drawingId == null) ? null : uuid(drawingId, "drawing_id");
		UUID before = (cursor == null || cursor.isEmpty()) ? null : decodeCursor(cursor);
		List<Listing> rows = this.listings.active(drawing, sellerId, before, limit + 1);
		List<Listing> page = rows.subList(0, Math.min(limit, rows.size()));
		String next = (rows.size() > limit) ? encodeCursor(page.getLast().listingId()) : null;
		return new ListingsPage(page, next);
	}

	/**
	 * Strict on purpose: a price is a JSON integer, never 1.5 or "5" (which a default object mapping
	 * would quietly turn into 1 and 5).
	 */
	private CreateListingRequest parse(String body) {
		JsonNode root;
		try {
			root = this.json.readTree(body == null ? "" : body);
		}
		catch (JacksonException ex) {
			throw ApiException.invalidRequest("the body must be {\"drawing_id\": \"<uuid>\", \"price\": <coins>}");
		}
		JsonNode drawing = (root == null) ? null : root.get("drawing_id");
		JsonNode price = (root == null) ? null : root.get("price");
		if (drawing == null || !drawing.isString() || price == null || !price.isIntegralNumber() || !price.canConvertToLong()) {
			throw ApiException.invalidRequest("drawing_id (a UUID) and price (whole coins) are required");
		}
		if (price.longValue() < 1 || price.longValue() > ListingService.MAX_PRICE) {
			throw ApiException.invalidRequest("price must be between 1 and " + ListingService.MAX_PRICE + " coins");
		}
		return new CreateListingRequest(uuid(drawing.stringValue(), "drawing_id"), price.longValue());
	}

	/** A malformed listing id gets the same answer as an unknown one. */
	static UUID parseId(String id) {
		try {
			return UUID.fromString(id);
		}
		catch (IllegalArgumentException ex) {
			throw ListingService.notFound();
		}
	}

	private static UUID uuid(String value, String field) {
		try {
			return UUID.fromString(value);
		}
		catch (IllegalArgumentException ex) {
			throw ApiException.invalidRequest(field + " must be a UUID");
		}
	}

	static String encodeCursor(UUID listingId) {
		return Base64.getUrlEncoder().withoutPadding().encodeToString(listingId.toString().getBytes(StandardCharsets.US_ASCII));
	}

	static UUID decodeCursor(String cursor) {
		try {
			return UUID.fromString(new String(Base64.getUrlDecoder().decode(cursor), StandardCharsets.US_ASCII));
		}
		catch (IllegalArgumentException ex) {
			throw ApiException.invalidRequest("invalid cursor");
		}
	}

	/** The body of {@code POST /listings}, checked. */
	record CreateListingRequest(UUID drawingId, long price) {
	}

	public record ListingsPage(List<Listing> listings, String nextCursor) {
	}

}
