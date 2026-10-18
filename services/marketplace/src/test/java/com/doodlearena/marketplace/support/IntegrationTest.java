package com.doodlearena.marketplace.support;

import java.lang.annotation.ElementType;
import java.lang.annotation.Retention;
import java.lang.annotation.RetentionPolicy;
import java.lang.annotation.Target;

import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.context.annotation.Import;

/**
 * The whole application against a real Postgres. Every class with this annotation shares one
 * Spring context and so one container; tests keep apart by using fresh user ids.
 */
@Target(ElementType.TYPE)
@Retention(RetentionPolicy.RUNTIME)
@SpringBootTest
@Import({ PostgresTestConfiguration.class, LedgerFixtures.class })
public @interface IntegrationTest {
}
