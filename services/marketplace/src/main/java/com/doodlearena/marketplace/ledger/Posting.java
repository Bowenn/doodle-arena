package com.doodlearena.marketplace.ledger;

import java.util.Objects;

/** One leg of a ledger transaction: a positive amount credits the account, a negative one debits it. */
public record Posting(AccountRef account, long amount) {

	public Posting {
		Objects.requireNonNull(account, "account");
	}

	public static Posting credit(AccountRef account, long amount) {
		return new Posting(account, amount);
	}

	public static Posting debit(AccountRef account, long amount) {
		return new Posting(account, Math.negateExact(amount));
	}

}
