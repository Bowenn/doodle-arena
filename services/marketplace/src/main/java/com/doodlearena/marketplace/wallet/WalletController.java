package com.doodlearena.marketplace.wallet;

import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.util.Base64;
import java.util.List;
import java.util.UUID;

import org.springframework.security.core.annotation.AuthenticationPrincipal;
import org.springframework.security.oauth2.jwt.Jwt;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;

import com.doodlearena.marketplace.api.ApiException;

/** The caller's own coins (design doc, section 6.3). The user is always the token's subject. */
@RestController
@RequestMapping("/api/market/wallet")
public class WalletController {

	private static final int DEFAULT_LIMIT = 20;

	private static final int MAX_LIMIT = 50;

	private final WalletRepository wallets;

	public WalletController(WalletRepository wallets) {
		this.wallets = wallets;
	}

	@GetMapping
	public WalletResponse wallet(@AuthenticationPrincipal Jwt token) {
		WalletRepository.Balance balance = this.wallets.balance(token.getSubject());
		return new WalletResponse(token.getSubject(), balance.balance(), balance.updatedAt());
	}

	@GetMapping("/transactions")
	public TransactionsResponse transactions(@AuthenticationPrincipal Jwt token,
			@RequestParam(defaultValue = "" + DEFAULT_LIMIT) int limit, @RequestParam(required = false) String cursor) {
		if (limit < 1 || limit > MAX_LIMIT) {
			throw ApiException.invalidRequest("limit must be between 1 and " + MAX_LIMIT);
		}
		Long before = (cursor == null || cursor.isEmpty()) ? null : decodeCursor(cursor);
		// One extra row says whether there is a next page.
		List<WalletRepository.Entry> rows = this.wallets.entries(token.getSubject(), before, limit + 1);
		List<WalletRepository.Entry> page = rows.subList(0, Math.min(limit, rows.size()));
		String next = (rows.size() > limit) ? encodeCursor(page.getLast().entryId()) : null;
		return new TransactionsResponse(page.stream()
			.map((entry) -> new TransactionItem(entry.txId(), entry.type(), entry.amount(), entry.createdAt()))
			.toList(), next);
	}

	static String encodeCursor(long entryId) {
		return Base64.getUrlEncoder().withoutPadding().encodeToString(Long.toString(entryId).getBytes(StandardCharsets.US_ASCII));
	}

	static long decodeCursor(String cursor) {
		try {
			long entryId = Long.parseLong(new String(Base64.getUrlDecoder().decode(cursor), StandardCharsets.US_ASCII));
			if (entryId <= 0) {
				throw ApiException.invalidRequest("invalid cursor");
			}
			return entryId;
		}
		catch (IllegalArgumentException ex) {
			throw ApiException.invalidRequest("invalid cursor");
		}
	}

	public record WalletResponse(String userId, long balance, Instant updatedAt) {
	}

	public record TransactionsResponse(List<TransactionItem> transactions, String nextCursor) {
	}

	/** One ledger entry of the caller: {@code amount} is positive for coins in, negative for coins out. */
	public record TransactionItem(UUID txId, String type, long amount, Instant createdAt) {
	}

}
