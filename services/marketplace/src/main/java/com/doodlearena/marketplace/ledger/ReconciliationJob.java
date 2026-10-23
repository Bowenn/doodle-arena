package com.doodlearena.marketplace.ledger;

import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Optional;
import java.util.TreeSet;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicLong;
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
 * <p>The ledger is partitioned by month (task 6.6, {@link LedgerMonths}): closed months count with
 * their stored per-account totals, and only the open months are scanned, so a run reads one or two
 * months instead of the whole ledger. Each run also checks one closed month's totals against its
 * partition again, so a change to old entries behind the triggers' back still shows, within as
 * many runs as there are closed months.
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

	private final LedgerMonths months;

	private final AtomicLong defaultPartitionRows = new AtomicLong(-1);

	public ReconciliationJob(JdbcClient db, PlatformTransactionManager transactions, Clock clock,
			MarketMetrics metrics, LedgerMonths months, @Value("${app.reconciliation.stale-after}") Duration staleAfter) {
		this.db = db;
		this.months = months;
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
		metrics.meter()
			.gaugeBuilder("market.tips.escrow_mismatch")
			.ofLongs()
			.setUnit("{shard}")
			.setDescription("Tips escrow shards whose balance isn't the sum of their pending tips; must be 0")
			.buildWithCallback((gauge) -> current().ifPresent((result) -> gauge.record(result.escrowMismatches())));
		metrics.meter()
			.gaugeBuilder("market.ledger.default_partition_rows")
			.ofLongs()
			.setUnit("{entry}")
			.setDescription("Ledger entries in the default partition, outside every monthly one; must be 0")
			.buildWithCallback((gauge) -> {
				long rows = this.defaultPartitionRows.get();
				if (rows >= 0 && current().isPresent()) {
					gauge.record(rows);
				}
			});
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
			span.setAttribute("tips.escrow_mismatch", result.escrowMismatches());
			var line = result.clean() ? log.atInfo() : log.atError();
			line.addKeyValue("imbalance", result.imbalance())
				.addKeyValue("mismatched_accounts", result.mismatchedAccounts())
				.addKeyValue("ownership_violations", result.ownershipViolations())
				.addKeyValue("escrow_mismatched_shards", result.escrowMismatchedShards())
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
		this.months.closeMonths();
		LedgerMonths.Verification verified = this.months.verifyOldest();
		this.defaultPartitionRows.set(this.months.defaultPartitionRows());
		ReconciliationResult result = this.snapshot.execute((status) -> {
			Instant checkedAt = this.clock.instant();
			// Closed months by their stored totals, open months (after the last closed one) by a
			// scan that Postgres prunes to their partitions.
			long imbalance = this.db.sql("""
					SELECT COALESCE((SELECT SUM(total) FROM ledger_month_totals), 0)
						+ COALESCE((SELECT SUM(amount) FROM ledger_entries WHERE created_at >= (%s)), 0)""".formatted(OPEN_FROM))
				.query(Long.class)
				.single();
			List<Long> mismatched = this.db.sql("""
					SELECT a.account_id
					FROM accounts a
					LEFT JOIN balances b USING (account_id)
					LEFT JOIN (
						SELECT account_id, SUM(total) AS total FROM (
							SELECT account_id, total FROM ledger_month_totals
							UNION ALL
							SELECT account_id, amount FROM ledger_entries WHERE created_at >= (%s)
						) x GROUP BY account_id) e
						USING (account_id)
					WHERE b.account_id IS NULL
						OR b.balance <> COALESCE(e.total, 0)
						OR (a.owner_type = 'user' AND b.balance < 0)
					ORDER BY a.account_id""".formatted(OPEN_FROM))
				.query(Long.class)
				.list();
			return new ReconciliationResult(imbalance, mismatched, ownershipViolations(), escrowMismatches(), checkedAt);
		});
		if (verified.imbalance() != 0 || !verified.mismatchedAccounts().isEmpty()) {
			// A closed month's entries no longer match their stored totals: something changed
			// them behind the triggers' back. Reported, never repaired.
			log.atError()
				.addKeyValue("month", String.valueOf(verified.month()))
				.addKeyValue("imbalance", verified.imbalance())
				.addKeyValue("mismatched_account_ids", verified.mismatchedAccounts().stream().limit(LOGGED_IDS).toList())
				.log("closed ledger month changed");
			TreeSet<Long> accounts = new TreeSet<>(result.mismatchedAccountIds());
			accounts.addAll(verified.mismatchedAccounts());
			result = new ReconciliationResult(result.imbalance() + verified.imbalance(), List.copyOf(accounts),
					result.ownershipViolationIds(), result.escrowMismatchedShards(), result.checkedAt());
		}
		this.last.set(result);
		return result;
	}

	/** The first moment of the open months: right after the last closed month, if any. */
	private static final String OPEN_FROM = """
			SELECT COALESCE(CAST(max(month) + interval '1 month' AS TIMESTAMPTZ), '-infinity')
			FROM ledger_months_closed""";

	/**
	 * Tips escrow shards (ADR 0021) that don't hold exactly their pending tips: a shard's coins
	 * belong to its pending tips and nothing else. Escrow accounts are system accounts, so no CHECK
	 * stops one going below zero; this does.
	 */
	private List<Integer> escrowMismatches() {
		return this.db.sql("""
				SELECT s.shard FROM (
					SELECT CAST(substr(a.owner_id, length('tips_escrow:') + 1) AS INT) AS shard, b.balance
					FROM accounts a JOIN balances b USING (account_id)
					WHERE a.owner_type = 'system' AND a.owner_id LIKE 'tips\\_escrow:%') s
				LEFT JOIN (SELECT shard, SUM(amount) AS pending FROM tips WHERE status = 'PENDING' GROUP BY shard) p
					USING (shard)
				WHERE s.balance <> COALESCE(p.pending, 0)
				ORDER BY s.shard""")
			.query(Integer.class)
			.list();
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
