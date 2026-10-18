package com.doodlearena.marketplace.support;

import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.webmvc.test.autoconfigure.AutoConfigureMockMvc;
import org.springframework.context.annotation.Import;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;

/**
 * The application behind its real security filter chain, verifying tokens from {@link TestTokens}.
 * API test classes extend this, so they share one Spring context and one database.
 */
@SpringBootTest
@AutoConfigureMockMvc
@Import({ PostgresTestConfiguration.class, LedgerFixtures.class })
public abstract class ApiTestSupport {

	@DynamicPropertySource
	static void jwks(DynamicPropertyRegistry registry) {
		registry.add("app.jwt.jwks-url", TestTokens::jwksUrl);
		registry.add("app.jwt.issuer", () -> TestTokens.ISSUER);
	}

}
