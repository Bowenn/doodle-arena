package com.doodlearena.marketplace.ledger;

import java.util.UUID;

/**
 * The outcome of {@link LedgerService#post}: the transaction's id, and whether this call found an
 * earlier transaction with the same idempotency key (a replay that changed nothing).
 */
public record PostResult(UUID txId, boolean replayed) {
}
