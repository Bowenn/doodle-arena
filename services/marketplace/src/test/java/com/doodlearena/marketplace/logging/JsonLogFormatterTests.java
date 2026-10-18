package com.doodlearena.marketplace.logging;

import java.time.Instant;
import java.util.Map;

import ch.qos.logback.classic.Level;
import ch.qos.logback.classic.Logger;
import ch.qos.logback.classic.LoggerContext;
import ch.qos.logback.classic.spi.LoggingEvent;
import ch.qos.logback.classic.util.LogbackMDCAdapter;
import org.junit.jupiter.api.Test;
import org.slf4j.event.KeyValuePair;

import org.springframework.boot.json.JsonParserFactory;
import org.springframework.mock.env.MockEnvironment;

import static org.assertj.core.api.Assertions.assertThat;

class JsonLogFormatterTests {

	private final JsonLogFormatter formatter = new JsonLogFormatter(
			new MockEnvironment().withProperty("spring.application.name", "marketplace")
				.withProperty("app.version", "1.2.3"));

	private final Logger logger = loggerContext().getLogger("test");

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
	void includesTraceContextFromMdc() {
		// The OpenTelemetry Java agent puts trace_id and span_id into the MDC.
		LoggingEvent event = new LoggingEvent("fqcn", this.logger, Level.INFO, "purchase completed", null, null);
		event.setMDCPropertyMap(Map.of("trace_id", "4bf92f3577b34da6a3ce929d0e0e4736", "span_id", "00f067aa0ba902b7"));

		Map<String, Object> json = JsonParserFactory.getJsonParser().parseMap(this.formatter.format(event));

		assertThat(json).containsEntry("trace_id", "4bf92f3577b34da6a3ce929d0e0e4736")
			.containsEntry("span_id", "00f067aa0ba902b7");
	}

	@Test
	void omitsTraceContextOutsideASpan() {
		LoggingEvent event = new LoggingEvent("fqcn", this.logger, Level.INFO, "started", null, null);
		event.setMDCPropertyMap(Map.of());

		Map<String, Object> json = JsonParserFactory.getJsonParser().parseMap(this.formatter.format(event));

		assertThat(json).doesNotContainKeys("trace_id", "span_id");
	}

	@Test
	void writesKeyValuePairsAsFields() {
		LoggingEvent event = new LoggingEvent("fqcn", this.logger, Level.INFO, "ledger transaction posted", null, null);
		event.addKeyValuePair(new KeyValuePair("user_id", "u_bob"));
		event.addKeyValuePair(new KeyValuePair("accounts", 2));

		Map<String, Object> json = JsonParserFactory.getJsonParser().parseMap(this.formatter.format(event));

		assertThat(json).containsEntry("user_id", "u_bob").containsEntry("accounts", "2");
	}

	// Like the context Spring Boot creates at runtime, which always has an MDC adapter.
	private static LoggerContext loggerContext() {
		LoggerContext context = new LoggerContext();
		context.setMDCAdapter(new LogbackMDCAdapter());
		return context;
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
