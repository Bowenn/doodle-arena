package com.doodlearena.marketplace.db;

import java.util.Optional;
import java.util.function.Function;
import java.util.regex.Pattern;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.metrics.LongCounter;
import jakarta.servlet.http.Cookie;
import jakarta.servlet.http.HttpServletRequest;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.dao.DataAccessException;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Component;
import org.springframework.web.context.request.RequestContextHolder;
import org.springframework.web.context.request.ServletRequestAttributes;

import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * Where a read-heavy endpoint reads (ADR 0020): the replica, unless the caller has written
 * something the replica hasn't replayed yet. A successful write answers with the primary's WAL
 * position in the cookie {@code da_rw_lsn} ({@link WritePositionFilter}); a read that carries it
 * goes to the replica only if the replica has replayed past it, so a user always sees their own
 * purchases, listings and coins. Without the cookie, the replica.
 *
 * <p>The replica never makes a read fail: one it can't answer is read from the primary instead.
 * Only reads outside a transaction come here; writes and the reads inside them use the primary.
 */
@Component
public final class ReadRouting {

	private static final Logger log = LoggerFactory.getLogger(ReadRouting.class);

	/** The cookie a write sets and a read sends back. */
	public static final String COOKIE = "da_rw_lsn";

	/** A pg_lsn as Postgres prints it: two hexadecimal halves. */
	private static final Pattern LSN = Pattern.compile("[0-9A-F]{1,8}/[0-9A-F]{1,8}");

	private static final AttributeKey<String> TARGET = AttributeKey.stringKey("target");

	private static final AttributeKey<String> REASON = AttributeKey.stringKey("reason");

	private final JdbcClient primary;

	private final ReadReplica replica;

	private final LongCounter reads;

	public ReadRouting(JdbcClient primary, ReadReplica replica, MarketMetrics metrics) {
		this.primary = primary;
		this.replica = replica;
		this.reads = metrics.meter()
			.counterBuilder("market.db.reads")
			.setDescription("Routed reads by target (primary or replica) and reason")
			.build();
	}

	/** Runs a read on the replica when it has the caller's writes, otherwise on the primary. */
	public <T> T read(Function<JdbcClient, T> query) {
		Optional<JdbcClient> replicaClient = this.replica.client();
		if (replicaClient.isEmpty()) {
			return on(this.primary, "primary", "no_replica", query);
		}
		String position = writePosition();
		try {
			if (position != null && !this.replica.replayedPast(position)) {
				return on(this.primary, "primary", "behind", query);
			}
			return on(replicaClient.get(), "replica", position == null ? "no_write" : "caught_up", query);
		}
		catch (DataAccessException ex) {
			log.atWarn().addKeyValue("error", ex.getMessage()).log("replica read failed: reading the primary");
			return on(this.primary, "primary", "replica_error", query);
		}
	}

	private <T> T on(JdbcClient db, String target, String reason, Function<JdbcClient, T> query) {
		this.reads.add(1, Attributes.of(TARGET, target, REASON, reason));
		return query.apply(db);
	}

	/** The caller's last write position, from the request's cookie, if it is a valid one. */
	private static String writePosition() {
		if (!(RequestContextHolder.getRequestAttributes() instanceof ServletRequestAttributes attributes)) {
			return null;
		}
		HttpServletRequest request = attributes.getRequest();
		if (request.getCookies() == null) {
			return null;
		}
		for (Cookie cookie : request.getCookies()) {
			if (COOKIE.equals(cookie.getName()) && LSN.matcher(cookie.getValue()).matches()) {
				return cookie.getValue();
			}
		}
		return null;
	}

}
