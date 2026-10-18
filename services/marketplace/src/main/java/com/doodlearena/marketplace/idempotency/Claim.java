package com.doodlearena.marketplace.idempotency;

import java.util.UUID;

/** A request's hold on its idempotency key. The token fences the completion (ADR 0012). */
public record Claim(String userId, String key, UUID token) {
}
