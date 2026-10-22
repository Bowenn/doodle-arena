package com.doodlearena.marketplace.db;

import java.io.IOException;
import java.util.Set;

import jakarta.servlet.FilterChain;
import jakarta.servlet.ServletException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.ResponseCookie;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Component;
import org.springframework.web.filter.OncePerRequestFilter;
import org.springframework.web.util.ContentCachingResponseWrapper;

/**
 * Read-your-writes, the writing half (ADR 0020): a successful write request answers with the
 * primary's WAL position after its commit in the cookie {@code da_rw_lsn}, which
 * {@link ReadRouting} compares with the replica's. The position is read once the request's
 * transaction has committed, so it is at or past the commit. Every service shares one Postgres
 * server, so its one WAL orders all of them.
 *
 * <p>The response is held until then (a write's body is small), because a cookie can't be added
 * to a response already sent. Only when a replica is configured.
 */
@Component
public final class WritePositionFilter extends OncePerRequestFilter {

	private static final Logger log = LoggerFactory.getLogger(WritePositionFilter.class);

	private static final Set<String> READS = Set.of("GET", "HEAD", "OPTIONS");

	private final JdbcClient primary;

	private final ReadReplica replica;

	private final boolean secure;

	public WritePositionFilter(JdbcClient primary, ReadReplica replica,
			@Value("${app.db.replica.cookie-secure:false}") boolean secure) {
		this.primary = primary;
		this.replica = replica;
		this.secure = secure;
	}

	@Override
	protected boolean shouldNotFilter(HttpServletRequest request) {
		return READS.contains(request.getMethod()) || this.replica.client().isEmpty()
				|| !request.getRequestURI().startsWith("/api/");
	}

	@Override
	protected void doFilterInternal(HttpServletRequest request, HttpServletResponse response, FilterChain chain)
			throws ServletException, IOException {
		ContentCachingResponseWrapper held = new ContentCachingResponseWrapper(response);
		try {
			chain.doFilter(request, held);
			if (held.getStatus() >= 200 && held.getStatus() < 300) {
				setPosition(held);
			}
		}
		finally {
			held.copyBodyToResponse();
		}
	}

	private void setPosition(HttpServletResponse response) {
		try {
			String position = this.primary.sql("SELECT CAST(pg_current_wal_lsn() AS TEXT)").query(String.class).single();
			response.addHeader("Set-Cookie", ResponseCookie.from(ReadRouting.COOKIE, position)
				.path("/api")
				.httpOnly(true)
				.secure(this.secure)
				.sameSite("Lax")
				.maxAge(3600)
				.build()
				.toString());
		}
		catch (RuntimeException ex) {
			// The write has committed; without the cookie the next read may miss it on the
			// replica for the moment the replica is behind.
			log.atWarn().addKeyValue("error", ex.getMessage()).log("write position not read");
		}
	}

}
