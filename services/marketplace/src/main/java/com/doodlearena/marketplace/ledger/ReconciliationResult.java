package com.doodlearena.marketplace.ledger;

import java.time.Instant;
import java.util.List;

/**
 * One run of the reconciliation job. Both numbers must be 0.
 * @param imbalance the sum of every ledger entry, in coins
 * @param mismatchedAccountIds accounts whose balance isn't the sum of their entries, users below
 * zero, and accounts without a balance row
 * @param checkedAt when the run's snapshot was taken
 */
public record ReconciliationResult(long imbalance, List<Long> mismatchedAccountIds, Instant checkedAt) {

	public long mismatchedAccounts() {
		return this.mismatchedAccountIds.size();
	}

	public boolean clean() {
		return this.imbalance == 0 && this.mismatchedAccountIds.isEmpty();
	}

}
