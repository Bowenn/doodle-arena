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

	public boolean isUser() {
		return this.ownerType.equals(USER);
	}

}
