package com.doodlearena.marketplace.db;

import java.util.Optional;
import java.util.concurrent.atomic.AtomicReference;

import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.DisposableBean;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * The Postgres read replica (ADR 0020), when {@code DB_REPLICA_HOST} is set: a small read-only pool
 * of its own. It isn't a DataSource bean, so Spring Boot still configures the primary as before
 * (Flyway, transactions, the JdbcClient every write uses). It connects as the marketplace's own
 * role, which the replica has because roles replicate.
 *
 * <p>It times out within a second, so a replica that is down costs a read a second at most before
 * {@link ReadRouting} reads the primary instead. Every 15 seconds it measures how far behind the
 * primary it is ({@code market.db.replica_lag}).
 */
@Component
public final class ReadReplica implements DisposableBean {

	private static final Logger log = LoggerFactory.getLogger(ReadReplica.class);

	private final HikariDataSource pool;

	private final JdbcClient db;

	private final AtomicReference<Double> lagSeconds = new AtomicReference<>();

	public ReadReplica(@Value("${app.db.replica.host:}") String host, @Value("${app.db.replica.port:5432}") int port,
			@Value("${spring.datasource.username}") String user, @Value("${spring.datasource.password}") String password,
			@Value("${DB_NAME:marketplace}") String name, MarketMetrics metrics) {
		if (host.isBlank()) {
			this.pool = null;
			this.db = null;
		}
		else {
			HikariConfig config = new HikariConfig();
			config.setPoolName("replica");
			config.setJdbcUrl("jdbc:postgresql://" + host + ":" + port + "/" + name);
			config.setUsername(user);
			config.setPassword(password);
			config.setReadOnly(true);
			// Names its sessions in pg_stat_activity (and tells tests which pool answered).
			config.addDataSourceProperty("ApplicationName", "marketplace-replica");
			config.setMaximumPoolSize(5);
			config.setMinimumIdle(0);
			config.setConnectionTimeout(1000);
			// The pool starts without a connection, so a replica that is down doesn't stop the service.
			config.setInitializationFailTimeout(-1);
			this.pool = new HikariDataSource(config);
			this.db = JdbcClient.create(this.pool);
		}
		metrics.meter()
			.gaugeBuilder("market.db.replica_lag")
			.setUnit("s")
			.setDescription("How far the read replica is behind the primary (0 when it has replayed everything it received)")
			.buildWithCallback((gauge) -> {
				Double lag = this.lagSeconds.get();
				if (lag != null) {
					gauge.record(lag);
				}
			});
	}

	/** The replica's client, if one is configured. */
	public Optional<JdbcClient> client() {
		return Optional.ofNullable(this.db);
	}

	/**
	 * Whether the replica has replayed the primary's WAL up to {@code position} (a pg_lsn such as
	 * {@code 0/16B3740}): then it holds everything committed before that position. A server that
	 * isn't a standby has no replay position and has everything it wrote (tests use one).
	 */
	public boolean replayedPast(String position) {
		Boolean done = this.db.sql("SELECT COALESCE(pg_last_wal_replay_lsn(), pg_current_wal_lsn()) >= CAST(? AS pg_lsn)")
			.param(position)
			.query(Boolean.class)
			.single();
		return Boolean.TRUE.equals(done);
	}

	/**
	 * Seconds behind: none while it has replayed all it received (an idle primary isn't lag),
	 * otherwise the age of the last transaction it replayed.
	 */
	@Scheduled(fixedDelayString = "${app.db.replica.lag-interval:15s}")
	public void measureLag() {
		if (this.db == null) {
			return;
		}
		try {
			this.lagSeconds.set(this.db.sql("""
					SELECT CASE WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
						ELSE COALESCE(EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()), 0) END""")
				.query(Double.class)
				.single());
		}
		catch (RuntimeException ex) {
			// No value rather than a wrong one: the replica can't be read.
			this.lagSeconds.set(null);
			log.atWarn().addKeyValue("error", ex.getMessage()).log("replica lag not measured");
		}
	}

	@Override
	public void destroy() {
		if (this.pool != null) {
			this.pool.close();
		}
	}

}
