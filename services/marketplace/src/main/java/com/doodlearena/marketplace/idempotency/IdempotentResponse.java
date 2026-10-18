package com.doodlearena.marketplace.idempotency;

import org.springframework.http.MediaType;
import org.springframework.http.ResponseEntity;

/**
 * An answer to an idempotent request, as JSON text: the stored text is what a replay sends, so a
 * replay is byte for byte the first answer.
 * @param replayed true when an earlier request with the same key produced it
 */
public record IdempotentResponse(int status, String body, boolean replayed) {

	public static final String REPLAYED_HEADER = "Idempotency-Replayed";

	public ResponseEntity<String> toResponseEntity() {
		var builder = ResponseEntity.status(this.status).contentType(MediaType.APPLICATION_JSON);
		if (this.replayed) {
			builder.header(REPLAYED_HEADER, "true");
		}
		return builder.body(this.body);
	}

}
