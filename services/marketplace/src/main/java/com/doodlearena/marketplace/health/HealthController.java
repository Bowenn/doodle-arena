package com.doodlearena.marketplace.health;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

/** Liveness endpoint used by the container healthcheck and the gateway's /healthz/marketplace. */
@RestController
public class HealthController {

	private final String version;
	private final String instance;

	public HealthController(@Value("${app.version}") String version, @Value("${app.instance}") String instance) {
		this.version = version;
		this.instance = instance;
	}

	@GetMapping("/healthz")
	public HealthResponse healthz() {
		return new HealthResponse("ok", "marketplace", version, instance);
	}

	public record HealthResponse(String status, String service, String version, String instance) {
	}

}
