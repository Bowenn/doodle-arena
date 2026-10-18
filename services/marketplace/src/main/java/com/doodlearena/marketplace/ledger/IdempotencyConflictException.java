package com.doodlearena.marketplace.ledger;

/** An idempotency key already belongs to a transaction of another type: a caller bug, never a replay. */
public class IdempotencyConflictException extends RuntimeException {

	private static final long serialVersionUID = 1L;

	public IdempotencyConflictException(String message) {
		super(message);
	}

}
