package com.doodlearena.marketplace.ledger;

import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Optional;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicReference;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * Checks the ledger's and ownership's invariants every minute (design doc, section 8) and exports
 * them as the gauges {@code market.ledger.imbalance}, {@code market.ledger.mismatched_accounts}
 * and {@code market.ownership.violations}, which alert when they aren't 0. Postgres already refuses unbalanced transactions and negative user
 * balances; this job catches whatever gets past that anyway: a disabled trigger, a balance edited
 * by hand, a bug in a later migration.
 *
 * <p>All queries run in one REPEATABLE READ, read-only transaction, so they see the same snapshot:
 * a posting or a sale that commits between them can't make the numbers disagree. Read-only work under
 * snapshot isolation takes no locks, so postings never wait for it.
 *
 * <p>When no run has succeeded for {@code app.reconciliation.stale-after}, the gauges report
 * nothing: a dead job shows as a missing series, never as a reassuring 0.
 */
@Component
public final class ReconciliationJob {

	private static final Logger log = LoggerFactory.getLogger(ReconciliationJob.class);

	private static final int LOGGED_IDS = 20;

	private final JdbcClient db;

	private final TransactionTemplate snapshot;

	private final Clock clock;

	private final Duration staleAfter;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	private final AtomicReference<ReconciliationResult> last = new AtomicReference<>();

	public ReconciliationJob(JdbcClient db, PlatformTransactionManager transactions, Clock clock,
			MarketMetrics metrics, @Value("${app.reconciliation.stale-after}") Duration staleAfter) {
		this.db = db;
		this.snapshot = new TransactionTemplate(transactions);
		this.snapshot.setIsolationLevel(TransactionDefinition.ISOLATION_REPEATABLE_READ);
		this.snapshot.setReadOnly(true);
		this.clock = clock;
		this.staleAfter = staleAfter;
		metrics.meter()
			.gaugeBuilder("market.ledger.imbalance")
			.ofLongs()
			.setUnit("{coin}")
			.setDescription("Sum of all ledger entries; must be 0")
			.buildWithCallback((gauge) -> current().ifPresent((result) -> gauge.record(result.imbalance())));
		metrics.meter()
			.gaugeBuilder("market.ledger.mismatched_accounts")
			.ofLongs()
			.setUnit("{account}")
			.setDescription("Accounts whose balance isn't the sum of their entries, or users below zero; must be 0")
			.buildWithCallback((gauge) -> current().ifPresent((result) -> gauge.record(result.mismatchedAccounts())));
		metrics.meter()
			.gaugeBuilder("market.ownership.violations")
			.ofLongs()
			.setUnit("{drawing}")
			.setDescription("Drawings whose ownership and provenance disagree; must be 0")
			.buildWithCallback((gauge) -> current().ifPresent((result) -> gauge.record(result.ownershipViolations())));
	}

	@Scheduled(fixedDelayString = "${app.reconciliation.interval}")
	@SuppressWarnings("try") // the scope only has to be open while the queries run
	public void run() {
		Span span = this.tracer.spanBuilder("market.reconcile").startSpan();
		try (Scope scope = span.makeCurrent()) {
			long started = System.nanoTime();
			ReconciliationResult result = reconcile();
			long durationMs = (System.nanoTime() - started) / 1_000_000;
			span.setAttribute("ledger.imbalance", result.imbalance());
			span.setAttribute("ledger.mismatched_accounts", result.mismatchedAccounts());
			span.setAttribute("ownership.violations", result.ownershipViolations());
			var line = result.clean() ? log.atInfo() : log.atError();
			line.addKeyValue("imbalance", result.imbalance())
				.addKeyValue("mismatched_accounts", result.mismatchedAccounts())
				.addKeyValue("ownership_violations", result.ownershipViolations())
				.addKeyValue("duration_ms", durationMs);
			if (!result.mismatchedAccountIds().isEmpty()) {
				line = line.addKeyValue("mismatched_account_ids",
						result.mismatchedAccountIds().stream().limit(LOGGED_IDS).toList());
			}
			if (!result.ownershipViolationIds().isEmpty()) {
				line = line.addKeyValue("violating_drawing_ids",
						result.ownershipViolationIds().stream().limit(LOGGED_IDS).map(UUID::toString).toList());
			}
			line.log(result.clean() ? "ledger reconciled" : "invariants broken");
		}
		catch (RuntimeException ex) {
			// The gauges keep the last result until it goes stale; the next run tries again.
			span.recordException(ex);
			span.setStatus(StatusCode.ERROR);
			log.error("ledger reconciliation failed", ex);
		}
		finally {
			span.end();
		}
	}

	/** Runs the checks once and makes the result the gauges' value. */
	public ReconciliationResult reconcile() {
		ReconciliationResult result = this.snapshot.execute((status) -> {
			Instant checkedAt = this.clock.instant();
			long imbalance = this.db.sql("SELECT COALESCE(SUM(amount), 0) FROM ledger_entries")
				.query(Long.class)
				.single();
			List<Long> mismatched = this.db.sql("""
					SELECT a.account_id
					FROM accounts a
					LEFT JOIN balances b USING (account_id)
					LEFT JOIN (SELECT account_id, SUM(amount) AS total FROM ledger_entries GROUP BY account_id) e
						USING (account_id)
					WHERE b.account_id IS NULL
						OR b.balance <> COALESCE(e.total, 0)
						OR (a.owner_type = 'user' AND b.balance < 0)
					ORDER BY a.account_id""")
				.query(Long.class)
				.list();
			return new ReconciliationResult(imbalance, mismatched, ownershipViolations(), checkedAt);
		});
		this.last.set(result);
		return result;
	}

	/**
	 * Drawings whose current owner and provenance disagree: not exactly one MINT event, a version
	 * that isn't the number of ownership events, an owner who isn't the latest event's new owner,
	 * no authors; plus events or authors of a drawing without an ownership row (the foreign keys
	 * forbid that, unless they were bypassed).
	 */
	private List<UUID> ownershipViolations() {
		return this.db.sql("""
				WITH ev AS (
					SELECT drawing_id,
						COUNT(*) AS events,
						COUNT(*) FILTER (WHERE kind = 'MINT') AS mints,
						(ARRAY_AGG(to_owner ORDER BY event_id DESC))[1] AS last_owner
					FROM ownership_events
					GROUP BY drawing_id
				)
				SELECT o.drawing_id
				FROM ownership o
				LEFT JOIN ev USING (drawing_id)
				WHERE ev.drawing_id IS NULL
					OR ev.mints <> 1
					OR ev.events <> o.version
					OR ev.last_owner <> o.owner_id
					OR NOT EXISTS (SELECT 1 FROM drawing_authors a WHERE a.drawing_id = o.drawing_id)
				UNION
				SELECT ev.drawing_id FROM ev
				WHERE NOT EXISTS (SELECT 1 FROM ownership o WHERE o.drawing_id = ev.drawing_id)
				UNION
				SELECT a.drawing_id FROM drawing_authors a
				WHERE NOT EXISTS (SELECT 1 FROM ownership o WHERE o.drawing_id = a.drawing_id)
				ORDER BY 1""")
			.query(UUID.class)
			.list();
	}

	/** The last result, unless it is too old to trust. */
	Optional<ReconciliationResult> current() {
		ReconciliationResult result = this.last.get();
		if (result == null || result.checkedAt().plus(this.staleAfter).isBefore(this.clock.instant())) {
			return Optional.empty();
		}
		return Optional.of(result);
	}

}
