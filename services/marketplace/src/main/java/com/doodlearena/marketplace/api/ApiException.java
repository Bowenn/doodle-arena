package com.doodlearena.marketplace.api;

import org.springframework.http.HttpStatus;

/** An error answered in the shared format: {"error": {"code", "message", "request_id"}}. */
public class ApiException extends RuntimeException {

	private static final long serialVersionUID = 1L;

	private final HttpStatus status;

	private final String code;

	public ApiException(HttpStatus status, String code, String message) {
		super(message);
		this.status = status;
		this.code = code;
	}

	public static ApiException invalidRequest(String message) {
		return new ApiException(HttpStatus.BAD_REQUEST, "INVALID_REQUEST", message);
	}

	public HttpStatus status() {
		return this.status;
	}

	public String code() {
		return this.code;
	}

}
