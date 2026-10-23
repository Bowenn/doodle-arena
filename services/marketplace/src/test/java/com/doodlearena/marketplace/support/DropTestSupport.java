package com.doodlearena.marketplace.support;

import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.boot.webmvc.test.autoconfigure.AutoConfigureMockMvc;
import org.springframework.context.annotation.Import;
import org.springframework.test.context.DynamicPropertyRegistry;
import org.springframework.test.context.DynamicPropertySource;

/**
 * Like {@link ApiTestSupport}, with a Redis for limited drops (ADR 0022). Drop test classes extend
 * this, so they share one context, one Postgres and one Redis.
 */
@SpringBootTest
@AutoConfigureMockMvc
@Import({ PostgresTestConfiguration.class, RedisTestConfiguration.class, LedgerFixtures.class })
public abstract class DropTestSupport {

	@DynamicPropertySource
	static void jwks(DynamicPropertyRegistry registry) {
		registry.add("app.jwt.jwks-url", TestTokens::jwksUrl);
		registry.add("app.jwt.issuer", () -> TestTokens.ISSUER);
	}

}
