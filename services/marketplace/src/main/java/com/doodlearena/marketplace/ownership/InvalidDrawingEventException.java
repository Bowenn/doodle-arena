package com.doodlearena.marketplace.ownership;

/** A drawing event that breaks its contract. Retrying can't help, so the consumer skips it. */
public class InvalidDrawingEventException extends RuntimeException {

	private static final long serialVersionUID = 1L;

	public InvalidDrawingEventException(String message) {
		super(message);
	}

}
