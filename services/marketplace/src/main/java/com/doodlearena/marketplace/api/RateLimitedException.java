package com.doodlearena.marketplace.api;

import org.springframework.http.HttpStatus;

/** 429 RATE_LIMITED, answered with Retry-After (whole seconds, at least 1). */
public class RateLimitedException extends ApiException {

	private static final long serialVersionUID = 1L;

	private final long retryAfterMs;

	public RateLimitedException(long retryAfterMs) {
		super(HttpStatus.TOO_MANY_REQUESTS, "RATE_LIMITED", "too many requests: try again in a moment");
		this.retryAfterMs = retryAfterMs;
	}

	public long retryAfterSeconds() {
		return Math.max(1, (this.retryAfterMs + 999) / 1000);
	}

}
