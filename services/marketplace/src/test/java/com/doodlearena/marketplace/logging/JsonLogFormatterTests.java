package com.doodlearena.marketplace.logging;

import java.time.Instant;
import java.util.Map;

import ch.qos.logback.classic.Level;
import ch.qos.logback.classic.Logger;
import ch.qos.logback.classic.LoggerContext;
import ch.qos.logback.classic.spi.LoggingEvent;
import org.junit.jupiter.api.Test;

import org.springframework.boot.json.JsonParserFactory;
import org.springframework.mock.env.MockEnvironment;

import static org.assertj.core.api.Assertions.assertThat;

class JsonLogFormatterTests {

	private final JsonLogFormatter formatter = new JsonLogFormatter(
			new MockEnvironment().withProperty("spring.application.name", "marketplace")
				.withProperty("app.version", "1.2.3"));

	private final Logger logger = new LoggerContext().getLogger("test");

	@Test
	void writesRequiredFields() {
		LoggingEvent event = new LoggingEvent("fqcn", this.logger, Level.WARN, "purchase {}", null,
				new Object[] { "conflict" });
		event.setTimeStamp(Instant.parse("2026-10-20T14:03:12Z").toEpochMilli());

		String line = this.formatter.format(event);

		assertThat(line).endsWith("\n");
		Map<String, Object> json = JsonParserFactory.getJsonParser().parseMap(line);
		assertThat(json).containsEntry("timestamp", "2026-10-20T14:03:12.000Z")
			.containsEntry("level", "warn")
			.containsEntry("service", "marketplace")
			.containsEntry("version", "1.2.3")
			.containsEntry("msg", "purchase conflict")
			.doesNotContainKey("exc");
	}

	@Test
	void includesStackTraceForExceptions() {
		LoggingEvent event = new LoggingEvent("fqcn", this.logger, Level.ERROR, "failed",
				new IllegalStateException("boom"), null);

		Map<String, Object> json = JsonParserFactory.getJsonParser().parseMap(this.formatter.format(event));

		assertThat(json.get("level")).isEqualTo("error");
		assertThat((String) json.get("exc")).contains("IllegalStateException: boom");
	}

}
