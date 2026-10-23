package com.doodlearena.marketplace.support;

import org.testcontainers.containers.GenericContainer;
import org.testcontainers.utility.DockerImageName;

import org.springframework.boot.test.context.TestConfiguration;
import org.springframework.boot.testcontainers.service.connection.ServiceConnection;
import org.springframework.context.annotation.Bean;

/**
 * A real Redis for the drop tests (ADR 0022), the same image as docker-compose. Only
 * {@link DropTestSupport} imports it, so the other tests keep their shared context.
 */
@TestConfiguration(proxyBeanMethods = false)
public class RedisTestConfiguration {

	@Bean
	@ServiceConnection(name = "redis")
	GenericContainer<?> redis() {
		return new GenericContainer<>(DockerImageName.parse("redis:8.10.2-trixie")).withExposedPorts(6379);
	}

}
