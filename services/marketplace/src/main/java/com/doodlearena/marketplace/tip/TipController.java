package com.doodlearena.marketplace.tip;

import jakarta.servlet.http.HttpServletRequest;
import tools.jackson.core.JacksonException;
import tools.jackson.databind.JsonNode;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.security.core.annotation.AuthenticationPrincipal;
import org.springframework.security.oauth2.jwt.Jwt;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestHeader;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.api.ErrorBody;
import com.doodlearena.marketplace.idempotency.IdempotencyService;

/** Tips (design doc, section 6.3; ADR 0021). The tipper is always the token's subject. */
@RestController
@RequestMapping("/api/market/tips")
public class TipController {

	/** Room and match ids are UUIDs from the lobby and game server; anything longer is not one. */
	private static final int MAX_ID_LENGTH = 128;

	private final TipService tips;

	private final IdempotencyService idempotency;

	private final JsonMapper json;

	public TipController(TipService tips, IdempotencyService idempotency, JsonMapper json) {
		this.tips = tips;
		this.idempotency = idempotency;
		this.json = json;
	}

	@PostMapping
	public ResponseEntity<String> tip(@AuthenticationPrincipal Jwt token,
			@RequestHeader(name = "Idempotency-Key", required = false) String key,
			@RequestBody(required = false) String body, HttpServletRequest request) {
		TipRequest input = parse(body);
		String tipper = token.getSubject();
		return this.idempotency
			.execute(tipper, key, "tip", IdempotencyService.hash("POST", request.getRequestURI(), body),
					ErrorBody.requestId(request), HttpStatus.CREATED,
					(claim) -> this.tips.tip(tipper, input.roomId(), input.matchId(), input.roundNo(), input.amount()))
			.toResponseEntity();
	}

	/** Strict like a listing's price: whole coins as a JSON integer, never 1.5 or "5". */
	private TipRequest parse(String body) {
		JsonNode root;
		try {
			root = this.json.readTree(body == null ? "" : body);
		}
		catch (JacksonException ex) {
			throw ApiException.invalidRequest(
					"the body must be {\"room_id\": ..., \"match_id\": ..., \"round_no\": <n>, \"amount\": <coins>}");
		}
		String roomId = id(root, "room_id");
		String matchId = id(root, "match_id");
		JsonNode roundNo = (root == null) ? null : root.get("round_no");
		if (roundNo == null || !roundNo.isIntegralNumber() || !roundNo.canConvertToInt() || roundNo.intValue() < 1) {
			throw ApiException.invalidRequest("round_no must be a whole number from 1");
		}
		JsonNode amount = (root == null) ? null : root.get("amount");
		if (amount == null || !amount.isIntegralNumber() || !amount.canConvertToLong() || amount.longValue() < 1
				|| amount.longValue() > TipService.MAX_AMOUNT) {
			throw ApiException.invalidRequest("amount must be whole coins from 1 to " + TipService.MAX_AMOUNT);
		}
		return new TipRequest(roomId, matchId, roundNo.intValue(), amount.longValue());
	}

	private static String id(JsonNode root, String field) {
		JsonNode node = (root == null) ? null : root.get(field);
		if (node == null || !node.isString() || node.stringValue().isBlank()
				|| node.stringValue().length() > MAX_ID_LENGTH) {
			throw ApiException.invalidRequest(field + " is required");
		}
		return node.stringValue();
	}

	/** The body of {@code POST /tips}, checked. */
	record TipRequest(String roomId, String matchId, int roundNo, long amount) {
	}

}
