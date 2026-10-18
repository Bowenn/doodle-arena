package com.doodlearena.marketplace.api;

import jakarta.servlet.http.HttpServletRequest;

/** The shared error format (design doc, section 6.3). The gateway sets X-Request-Id. */
public record ErrorBody(Detail error) {

	public record Detail(String code, String message, String requestId) {
	}

	public static ErrorBody of(String code, String message, HttpServletRequest request) {
		String requestId = request.getHeader("X-Request-Id");
		return new ErrorBody(new Detail(code, message, (requestId != null) ? requestId : ""));
	}

}
