package com.doodlearena.marketplace.ledger;

/** A posting would take a user's balance below zero. Nothing was written. */
public class InsufficientFundsException extends RuntimeException {

	private static final long serialVersionUID = 1L;

	private final transient AccountRef account;

	public InsufficientFundsException(AccountRef account, long balance, long change) {
		super("balance " + balance + " can't cover " + change);
		this.account = account;
	}

	public AccountRef account() {
		return this.account;
	}

}
