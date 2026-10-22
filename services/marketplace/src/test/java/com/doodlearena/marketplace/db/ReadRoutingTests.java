package com.doodlearena.marketplace.db;

import java.net.URI;

import javax.sql.DataSource;

import com.zaxxer.hikari.HikariDataSource;
import jakarta.servlet.http.Cookie;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.mock.web.MockFilterChain;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.mock.web.MockHttpServletResponse;
import org.springframework.web.context.request.RequestContextHolder;
import org.springframework.web.context.request.ServletRequestAttributes;

import com.doodlearena.marketplace.support.IntegrationTest;
import com.doodlearena.marketplace.telemetry.MarketMetrics;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * Read-your-writes (ADR 0020). The test's Postgres also stands in for the replica (a server that
 * isn't a standby has replayed everything it wrote); the replica's pool names its sessions, so
 * each test can tell which pool answered.
 */
@IntegrationTest
class ReadRoutingTests {

	private static final String WHO = "SELECT current_setting('application_name')";

	@Autowired
	private JdbcClient primary;

	@Autowired
	private DataSource dataSource;

	@Autowired
	private MarketMetrics metrics;

	@AfterEach
	void forgetRequest() {
		RequestContextHolder.resetRequestAttributes();
	}

	private ReadReplica replicaAt(String host, int port) {
		HikariDataSource hikari = (HikariDataSource) this.dataSource;
		URI url = URI.create(hikari.getJdbcUrl().substring("jdbc:".length()));
		return new ReadReplica(host == null ? url.getHost() : host, port == 0 ? url.getPort() : port, hikari.getUsername(),
				hikari.getPassword(), url.getPath().substring(1).replaceAll("\\?.*", ""), this.metrics);
	}

	private String answeredBy(ReadReplica replica) {
		return new ReadRouting(this.primary, replica, this.metrics).read((db) -> db.sql(WHO).query(String.class).single());
	}

	private static void requestWithPosition(String position) {
		MockHttpServletRequest request = new MockHttpServletRequest("GET", "/api/market/listings");
		if (position != null) {
			request.setCookies(new Cookie(ReadRouting.COOKIE, position));
		}
		RequestContextHolder.setRequestAttributes(new ServletRequestAttributes(request));
	}

	@Test
	void withoutAReplicaEveryReadIsThePrimarys() {
		ReadReplica none = new ReadReplica("", 0, "u", "p", "d", this.metrics);
		assertThat(answeredBy(none)).isNotEqualTo("marketplace-replica");
		none.destroy();
	}

	@Test
	void aCallerWithoutWritesReadsTheReplica() {
		ReadReplica replica = replicaAt(null, 0);
		requestWithPosition(null);
		assertThat(answeredBy(replica)).isEqualTo("marketplace-replica");
		replica.destroy();
	}

	@Test
	void aWriteTheReplicaHasntReplayedSendsTheReadToThePrimary() {
		ReadReplica replica = replicaAt(null, 0);
		requestWithPosition("FFFFFFFF/FFFFFFFF");
		assertThat(answeredBy(replica)).isNotEqualTo("marketplace-replica");
		replica.destroy();
	}

	@Test
	void aWriteTheReplicaHasReplayedReadsTheReplica() {
		ReadReplica replica = replicaAt(null, 0);
		String now = this.primary.sql("SELECT CAST(pg_current_wal_lsn() AS TEXT)").query(String.class).single();
		requestWithPosition(now);
		assertThat(answeredBy(replica)).isEqualTo("marketplace-replica");
		replica.destroy();
	}

	@Test
	void aCookieThatIsntAPositionIsIgnored() {
		ReadReplica replica = replicaAt(null, 0);
		requestWithPosition("0/0'; DROP TABLE listings; --");
		assertThat(answeredBy(replica)).isEqualTo("marketplace-replica");
		replica.destroy();
	}

	@Test
	void aReplicaThatIsDownNeverFailsARead() {
		ReadReplica down = replicaAt("127.0.0.1", 1);
		long started = System.nanoTime();
		assertThat(answeredBy(down)).isNotEqualTo("marketplace-replica");
		assertThat((System.nanoTime() - started) / 1_000_000).as("ms to fall back").isLessThan(5000);
		down.destroy();
	}

	@Test
	void aSuccessfulWriteAnswersWithItsPosition() throws Exception {
		ReadReplica replica = replicaAt(null, 0);
		WritePositionFilter filter = new WritePositionFilter(this.primary, replica, true);

		MockHttpServletResponse created = new MockHttpServletResponse();
		filter.doFilter(new MockHttpServletRequest("POST", "/api/market/listings"), created, new MockFilterChain() {
			@Override
			public void doFilter(jakarta.servlet.ServletRequest req, jakarta.servlet.ServletResponse res) {
				((jakarta.servlet.http.HttpServletResponse) res).setStatus(201);
			}
		});
		assertThat(created.getHeader("Set-Cookie")).matches("da_rw_lsn=[0-9A-F]+/[0-9A-F]+; Path=/api; Max-Age=3600; .*")
			.contains("Secure", "HttpOnly", "SameSite=Lax");

		MockHttpServletResponse refused = new MockHttpServletResponse();
		filter.doFilter(new MockHttpServletRequest("POST", "/api/market/listings"), refused, new MockFilterChain() {
			@Override
			public void doFilter(jakarta.servlet.ServletRequest req, jakarta.servlet.ServletResponse res) {
				((jakarta.servlet.http.HttpServletResponse) res).setStatus(409);
			}
		});
		assertThat(refused.getHeader("Set-Cookie")).isNull();

		MockHttpServletResponse read = new MockHttpServletResponse();
		filter.doFilter(new MockHttpServletRequest("GET", "/api/market/listings"), read, new MockFilterChain());
		assertThat(read.getHeader("Set-Cookie")).isNull();
		replica.destroy();
	}

}
