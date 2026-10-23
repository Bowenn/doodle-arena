package com.doodlearena.marketplace.tip;

/** A RoundFinished that breaks its contract. Retrying can't help, so the consumer skips it. */
public class InvalidRoundEventException extends RuntimeException {

	private static final long serialVersionUID = 1L;

	public InvalidRoundEventException(String message) {
		super(message);
	}

}
