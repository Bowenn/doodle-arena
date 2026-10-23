package com.doodlearena.marketplace.ledger;

import java.util.Objects;

/** Names an account by its owner: a user id from auth, or a system account. */
public record AccountRef(String ownerType, String ownerId) {

	public static final String USER = "user";

	public static final String SYSTEM = "system";

	/** Coins bought with Stripe come from here (design doc, section 7.7). */
	public static final AccountRef STRIPE_CLEARING = new AccountRef(SYSTEM, "stripe_clearing");

	/** Round rewards come from here (design doc, section 7.9). */
	public static final AccountRef REWARDS_ISSUANCE = new AccountRef(SYSTEM, "rewards_issuance");

	/** Tips wait here until their round finishes (ADR 0021); migration V10 creates the shards. */
	public static final int TIPS_ESCROW_SHARDS = 16;

	/** A limited drop's sales wait here until its authors are paid (ADR 0022); V11 creates them. */
	public static final int DROP_ESCROW_SHARDS = 16;

	public AccountRef {
		Objects.requireNonNull(ownerType, "ownerType");
		Objects.requireNonNull(ownerId, "ownerId");
		if (!ownerType.equals(USER) && !ownerType.equals(SYSTEM)) {
			throw new IllegalArgumentException("unknown owner type " + ownerType);
		}
		if (ownerId.isBlank()) {
			throw new IllegalArgumentException("owner id is blank");
		}
	}

	public static AccountRef user(String userId) {
		return new AccountRef(USER, userId);
	}

	/** One of the tips escrow shards, 0 to {@link #TIPS_ESCROW_SHARDS} - 1. */
	public static AccountRef tipsEscrow(int shard) {
		if (shard < 0 || shard >= TIPS_ESCROW_SHARDS) {
			throw new IllegalArgumentException("no tips escrow shard " + shard);
		}
		return new AccountRef(SYSTEM, "tips_escrow:" + shard);
	}

	/** One of the drop escrow shards, 0 to {@link #DROP_ESCROW_SHARDS} - 1. */
	public static AccountRef dropEscrow(int shard) {
		if (shard < 0 || shard >= DROP_ESCROW_SHARDS) {
			throw new IllegalArgumentException("no drop escrow shard " + shard);
		}
		return new AccountRef(SYSTEM, "drop_escrow:" + shard);
	}

	public boolean isUser() {
		return this.ownerType.equals(USER);
	}

}
