package com.doodlearena.marketplace.logging;

import java.time.Instant;
import java.time.ZoneOffset;
import java.time.format.DateTimeFormatter;
import java.util.Collection;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;

import ch.qos.logback.classic.spi.ILoggingEvent;
import ch.qos.logback.classic.spi.ThrowableProxyUtil;
import org.slf4j.event.KeyValuePair;

import org.springframework.boot.json.JsonWriter;
import org.springframework.boot.logging.structured.StructuredLogFormatter;
import org.springframework.core.env.Environment;

/**
 * Writes one JSON object per log event with the required fields timestamp, level, service,
 * version, trace_id, span_id and msg (docs/observability.md, section 2). The OpenTelemetry Java
 * agent puts trace_id and span_id into the MDC while a span is active; outside a span (for example
 * at startup) both are left out. Domain fields (user_id, drawing_id, tx_id, ...) come from SLF4J's
 * key-value pairs: {@code log.atInfo().addKeyValue("user_id", id).log("...")}.
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
			members.add("trace_id", (event) -> event.getMDCPropertyMap().get("trace_id")).whenNotNull();
			members.add("span_id", (event) -> event.getMDCPropertyMap().get("span_id")).whenNotNull();
			members.add("msg", ILoggingEvent::getFormattedMessage);
			members.addMapEntries(JsonLogFormatter::keyValues);
			members.add("logger", ILoggingEvent::getLoggerName);
			members.add("exc", ILoggingEvent::getThrowableProxy).whenNotNull().as(ThrowableProxyUtil::asString);
		}).withNewLineAtEnd();
	}

	private static Map<String, Object> keyValues(ILoggingEvent event) {
		Map<String, Object> fields = new LinkedHashMap<>();
		List<KeyValuePair> pairs = event.getKeyValuePairs();
		if (pairs != null) {
			for (KeyValuePair pair : pairs) {
				// Numbers, booleans and lists stay JSON numbers, booleans and arrays; anything else becomes
				// its string.
				Object value = pair.value;
				boolean json = value == null || value instanceof Number || value instanceof Boolean
						|| value instanceof Collection<?>;
				fields.put(pair.key, json ? value : value.toString());
			}
		}
		return fields;
	}

	@Override
	public String format(ILoggingEvent event) {
		return this.writer.writeToString(event);
	}

}
