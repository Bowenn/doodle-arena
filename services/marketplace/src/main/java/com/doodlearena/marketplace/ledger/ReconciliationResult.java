package com.doodlearena.marketplace.ledger;

import java.time.Instant;
import java.util.List;
import java.util.UUID;

/**
 * One run of the reconciliation job. All five numbers must be 0.
 * @param imbalance the sum of every ledger entry, in coins
 * @param mismatchedAccountIds accounts whose balance isn't the sum of their entries, users below
 * zero, and accounts without a balance row
 * @param ownershipViolationIds drawings whose ownership and provenance disagree (design doc, section 8)
 * @param escrowMismatchedShards tips escrow shards whose balance isn't the sum of their pending tips
 * (ADR 0021)
 * @param dropEscrowMismatchedShards drop escrow shards whose balance isn't the sum of their paid
 * editions (ADR 0022)
 * @param checkedAt when the run's snapshot was taken
 */
public record ReconciliationResult(long imbalance, List<Long> mismatchedAccountIds, List<UUID> ownershipViolationIds,
		List<Integer> escrowMismatchedShards, List<Integer> dropEscrowMismatchedShards, Instant checkedAt) {

	public long mismatchedAccounts() {
		return this.mismatchedAccountIds.size();
	}

	public long ownershipViolations() {
		return this.ownershipViolationIds.size();
	}

	public long escrowMismatches() {
		return this.escrowMismatchedShards.size();
	}

	public long dropEscrowMismatches() {
		return this.dropEscrowMismatchedShards.size();
	}

	public boolean clean() {
		return this.imbalance == 0 && this.mismatchedAccountIds.isEmpty() && this.ownershipViolationIds.isEmpty()
				&& this.escrowMismatchedShards.isEmpty() && this.dropEscrowMismatchedShards.isEmpty();
	}

}
