package com.doodlearena.marketplace.logging;

import java.time.Instant;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;
import java.util.Locale;

import ch.qos.logback.classic.spi.ILoggingEvent;
import ch.qos.logback.classic.spi.ThrowableProxyUtil;

import org.springframework.boot.json.JsonWriter;
import org.springframework.boot.logging.structured.StructuredLogFormatter;
import org.springframework.core.env.Environment;

/**
 * Writes one JSON object per log event with the required fields timestamp, level, service,
 * version and msg (docs/observability.md, section 2). trace_id and span_id are added once
 * OpenTelemetry is wired in (task 1.5).
 */
public class JsonLogFormatter implements StructuredLogFormatter<ILoggingEvent> {

	private static final DateTimeFormatter TIMESTAMP =
			DateTimeFormatter.ofPattern("yyyy-MM-dd'T'HH:mm:ss.SSSX").withZone(ZoneOffset.UTC);

	private final JsonWriter<ILoggingEvent> writer;

	public JsonLogFormatter(Environment environment) {
		String service = environment.getProperty("spring.application.name", "marketplace");
		String version = environment.getProperty("app.version", "dev");
		this.writer = JsonWriter.<ILoggingEvent>of((members) -> {
			members.add("timestamp", (event) -> TIMESTAMP.format(Instant.ofEpochMilli(event.getTimeStamp())));
			members.add("level", (event) -> event.getLevel().toString().toLowerCase(Locale.ROOT));
			members.add("service", service);
			members.add("version", version);
			members.add("msg", ILoggingEvent::getFormattedMessage);
			members.add("logger", ILoggingEvent::getLoggerName);
			members.add("exc", ILoggingEvent::getThrowableProxy).whenNotNull().as(ThrowableProxyUtil::asString);
		}).withNewLineAtEnd();
	}

	@Override
	public String format(ILoggingEvent event) {
		return this.writer.writeToString(event);
	}

}
