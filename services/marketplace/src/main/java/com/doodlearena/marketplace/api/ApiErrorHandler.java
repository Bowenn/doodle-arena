package com.doodlearena.marketplace.api;

import jakarta.servlet.http.HttpServletRequest;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.HttpRequestMethodNotSupportedException;
import org.springframework.web.bind.MissingServletRequestParameterException;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.RestControllerAdvice;
import org.springframework.web.method.annotation.MethodArgumentTypeMismatchException;
import org.springframework.web.servlet.resource.NoResourceFoundException;

/** Turns exceptions into the shared error format, so no endpoint answers with Spring's default page. */
@RestControllerAdvice
public class ApiErrorHandler {

	private static final Logger log = LoggerFactory.getLogger(ApiErrorHandler.class);

	@ExceptionHandler(ApiException.class)
	ResponseEntity<ErrorBody> api(ApiException ex, HttpServletRequest request) {
		ResponseEntity.BodyBuilder response = ResponseEntity.status(ex.status());
		if (ex instanceof RateLimitedException limited) {
			response.header("Retry-After", Long.toString(limited.retryAfterSeconds()));
		}
		return response.body(ErrorBody.of(ex.code(), ex.getMessage(), request));
	}

	@ExceptionHandler({ MethodArgumentTypeMismatchException.class, MissingServletRequestParameterException.class })
	ResponseEntity<ErrorBody> badParameter(Exception ex, HttpServletRequest request) {
		return ResponseEntity.badRequest().body(ErrorBody.of("INVALID_REQUEST", "invalid query parameter", request));
	}

	@ExceptionHandler(NoResourceFoundException.class)
	ResponseEntity<ErrorBody> notFound(HttpServletRequest request) {
		return ResponseEntity.status(HttpStatus.NOT_FOUND).body(ErrorBody.of("NOT_FOUND", "no such endpoint", request));
	}

	@ExceptionHandler(HttpRequestMethodNotSupportedException.class)
	ResponseEntity<ErrorBody> methodNotAllowed(HttpServletRequest request) {
		return ResponseEntity.status(HttpStatus.METHOD_NOT_ALLOWED)
			.body(ErrorBody.of("METHOD_NOT_ALLOWED", "method not allowed", request));
	}

	@ExceptionHandler(Exception.class)
	ResponseEntity<ErrorBody> unexpected(Exception ex, HttpServletRequest request) {
		log.error("request failed", ex);
		return ResponseEntity.status(HttpStatus.INTERNAL_SERVER_ERROR)
			.body(ErrorBody.of("INTERNAL", "internal error", request));
	}

}
