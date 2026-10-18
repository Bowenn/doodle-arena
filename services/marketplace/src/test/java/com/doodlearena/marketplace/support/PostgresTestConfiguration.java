package com.doodlearena.marketplace.support;

import org.testcontainers.postgresql.PostgreSQLContainer;
import org.testcontainers.utility.DockerImageName;

import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.boot.testcontainers.service.connection.ServiceConnection;
import org.springframework.context.annotation.Bean;

/**
 * A real Postgres for integration tests (rule: .claude/rules/java.md), the same image as
 * docker-compose. Spring caches the test context, so test classes that import this share one
 * container. Its user is a superuser, which lets tests bypass the ledger's triggers on purpose.
 */
@TestConfiguration(proxyBeanMethods = false)
public class PostgresTestConfiguration {

	public static final DockerImageName IMAGE = DockerImageName.parse("postgres:18.6-trixie");

	@Bean
	@ServiceConnection
	PostgreSQLContainer postgres() {
		return new PostgreSQLContainer(IMAGE);
	}

}
