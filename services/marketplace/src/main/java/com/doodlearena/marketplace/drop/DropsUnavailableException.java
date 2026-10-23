package com.doodlearena.marketplace.drop;

/** Redis, the drops' gate, can't be reached or failed (ADR 0022): drops answer 503, never guess. */
public class DropsUnavailableException extends RuntimeException {

	private static final long serialVersionUID = 1L;

	public DropsUnavailableException(String message, Throwable cause) {
		super(message, cause);
	}

}
