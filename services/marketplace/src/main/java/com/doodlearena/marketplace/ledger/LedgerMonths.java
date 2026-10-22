package com.doodlearena.marketplace.ledger;

import java.time.LocalDate;
import java.util.List;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.boot.context.event.ApplicationReadyEvent;
import org.springframework.context.event.EventListener;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;

/**
 * The ledger's months (task 6.6): {@code ledger_entries} is partitioned by month (V8).
 *
 * <ul>
 * <li>Partitions are created three months ahead, at startup and daily, so postings never land in
 * the default partition (a month whose rows are there can't get its own partition any more).
 * <li>A month closes a day after it ends: its per-account totals go to
 * {@code ledger_month_totals} once, and the reconciliation job adds them to a scan of the open
 * months only, instead of summing the whole ledger every minute.
 * <li>Cached totals can't see a change made to old entries behind the triggers' back, so every
 * run checks one closed month against its partition again, the one checked longest ago: every
 * month within as many runs as there are closed months.
 * </ul>
 */
@Component
public final class LedgerMonths {

	private static final Logger log = LoggerFactory.getLogger(LedgerMonths.class);

	/** Months of partitions kept ahead of the current one. */
	static final int MONTHS_AHEAD = 3;

	private final JdbcClient db;

	private final TransactionTemplate tx;

	public LedgerMonths(JdbcClient db, PlatformTransactionManager transactions) {
		this.db = db;
		this.tx = new TransactionTemplate(transactions);
	}

	/** One closed month's totals checked again: accounts whose live sum differs, and by how much. */
	public record Verification(LocalDate month, List<Long> mismatchedAccounts, long imbalance) {

		static Verification none() {
			return new Verification(null, List.of(), 0);
		}

	}

	@EventListener(ApplicationReadyEvent.class)
	@Scheduled(cron = "${app.ledger.partitions-cron:0 15 3 * * *}")
	public void ensurePartitions() {
		Integer created = this.db.sql("SELECT ledger_ensure_partitions(CAST(now() AS DATE), ?)")
			.param(MONTHS_AHEAD)
			.query(Integer.class)
			.single();
		if (created != null && created > 0) {
			log.atInfo().addKeyValue("created", created).log("ledger partitions created");
		}
	}

	/** Rows in the default partition: postings outside every monthly partition. Must be 0. */
	public long defaultPartitionRows() {
		Long rows = this.db.sql("SELECT count(*) FROM ledger_entries_default").query(Long.class).single();
		return rows == null ? 0 : rows;
	}

	/**
	 * Closes every month that ended more than a day ago and isn't closed yet, oldest first, each
	 * in its own transaction. Returns how many it closed.
	 */
	public int closeMonths() {
		int closed = 0;
		while (true) {
			LocalDate month = this.db.sql("""
					SELECT CAST(m AS DATE) FROM (
						SELECT COALESCE(
							(SELECT max(month) + interval '1 month' FROM ledger_months_closed),
							(SELECT date_trunc('month', min(created_at)) FROM ledger_entries)) AS m) x
					WHERE m + interval '1 month' <= now() - interval '1 day'""")
				.query(LocalDate.class)
				.optional()
				.orElse(null);
			if (month == null) {
				return closed;
			}
			this.tx.executeWithoutResult((status) -> {
				long entries = this.db.sql("""
						SELECT count(*) FROM ledger_entries
						WHERE created_at >= CAST(? AS DATE) AND created_at < CAST(? AS DATE) + interval '1 month'""")
					.params(month, month)
					.query(Long.class)
					.single();
				this.db.sql("INSERT INTO ledger_months_closed (month, entries) VALUES (?, ?)").params(month, entries).update();
				this.db.sql("""
						INSERT INTO ledger_month_totals (month, account_id, total)
						SELECT ?, account_id, SUM(amount) FROM ledger_entries
						WHERE created_at >= CAST(? AS DATE) AND created_at < CAST(? AS DATE) + interval '1 month'
						GROUP BY account_id""")
					.params(month, month, month)
					.update();
			});
			log.atInfo().addKeyValue("month", month.toString()).log("ledger month closed");
			closed++;
		}
	}

	/**
	 * Checks the closed month checked longest ago against its partition again, and marks it
	 * checked. A difference is what a change behind the triggers' back leaves: it is reported,
	 * never repaired.
	 */
	public Verification verifyOldest() {
		Verification result = this.tx.execute((status) -> {
			LocalDate month = this.db.sql("SELECT month FROM ledger_months_closed ORDER BY verified_at, month LIMIT 1 FOR UPDATE SKIP LOCKED")
				.query(LocalDate.class)
				.optional()
				.orElse(null);
			if (month == null) {
				return Verification.none();
			}
			List<Long> accounts = this.db.sql("""
					SELECT COALESCE(l.account_id, c.account_id) FROM (
						SELECT account_id, SUM(amount) AS total FROM ledger_entries
						WHERE created_at >= CAST(:m AS DATE) AND created_at < CAST(:m AS DATE) + interval '1 month'
						GROUP BY account_id) l
					FULL JOIN (SELECT account_id, total FROM ledger_month_totals WHERE month = :m) c USING (account_id)
					WHERE l.total IS DISTINCT FROM c.total
					ORDER BY 1""")
				.param("m", month)
				.query(Long.class)
				.list();
			long imbalance = this.db.sql("""
					SELECT COALESCE((SELECT SUM(amount) FROM ledger_entries
						WHERE created_at >= CAST(:m AS DATE) AND created_at < CAST(:m AS DATE) + interval '1 month'), 0)
						- COALESCE((SELECT SUM(total) FROM ledger_month_totals WHERE month = :m), 0)""")
				.param("m", month)
				.query(Long.class)
				.single();
			this.db.sql("UPDATE ledger_months_closed SET verified_at = now() WHERE month = ?").param(month).update();
			return new Verification(month, accounts, imbalance);
		});
		return result == null ? Verification.none() : result;
	}

}
