package com.doodlearena.marketplace.tip;

import java.time.Instant;
import java.util.UUID;

/**
 * A tip, as {@code POST /api/market/tips} answers it (ADR 0021). {@code status} is PENDING until
 * its round's RoundFinished pays it to the drawers (SETTLED) or it goes back to the tipper
 * (REFUNDED).
 */
public record Tip(UUID tipId, String roomId, String matchId, int roundNo, long amount, String status,
		Instant createdAt) {

	static final String COLUMNS = "tip_id, room_id, match_id, round_no, amount, status, created_at";

}
