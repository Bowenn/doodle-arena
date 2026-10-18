package com.doodlearena.marketplace.reward;

/** A MatchFinished that breaks its contract. Retrying can't help, so the consumer skips it. */
public class InvalidMatchEventException extends RuntimeException {

	private static final long serialVersionUID = 1L;

	public InvalidMatchEventException(String message) {
		super(message);
	}

}
