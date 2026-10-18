package com.doodlearena.marketplace.stripe;

import java.util.List;

import jakarta.servlet.http.HttpServletRequest;

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
import org.springframework.web.bind.annotation.RestController;

import com.doodlearena.marketplace.api.ErrorBody;
import com.doodlearena.marketplace.idempotency.IdempotencyService;
import com.doodlearena.marketplace.stripe.StripeProperties.CoinPack;

/** Coin packs and their Stripe Checkout (design doc, section 6.3). */
@RestController
@RequestMapping("/api/market/coin-packs")
public class CoinPackController {

	private final StripeProperties properties;

	private final CheckoutService checkouts;

	private final IdempotencyService idempotency;

	public CoinPackController(StripeProperties properties, CheckoutService checkouts, IdempotencyService idempotency) {
		this.properties = properties;
		this.checkouts = checkouts;
		this.idempotency = idempotency;
	}

	@GetMapping
	public CoinPacks packs() {
		return new CoinPacks(this.properties.coinPacks()
			.stream()
			.map((pack) -> new CoinPackView(pack.id(), pack.coins(), pack.priceCents(), pack.currency()))
			.toList());
	}

	@PostMapping("/{id}/checkout")
	public ResponseEntity<String> checkout(@AuthenticationPrincipal Jwt token, @PathVariable String id,
			@RequestHeader(name = "Idempotency-Key", required = false) String key,
			@RequestBody(required = false) String body, HttpServletRequest request) {
		CoinPack pack = this.checkouts.pack(id);
		if (!this.properties.configured()) {
			throw StripeGateway.notConfigured();
		}
		String user = token.getSubject();
		return this.idempotency
			.execute(user, key, "checkout", IdempotencyService.hash("POST", request.getRequestURI(), body),
					ErrorBody.requestId(request), HttpStatus.OK,
					// Stripe is called with no transaction open; only the record is in the completing one.
					(claim) -> this.checkouts.createSession(user, pack, claim),
					(claim, checkout) -> this.checkouts.record(user, pack, checkout))
			.toResponseEntity();
	}

	public record CoinPacks(List<CoinPackView> packs) {
	}

	public record CoinPackView(String packId, long coins, long priceCents, String currency) {
	}

}
