package com.doodlearena.marketplace.ledger;

import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import javax.sql.DataSource;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.context.annotation.Import;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.jdbc.datasource.DataSourceTransactionManager;
import org.springframework.jdbc.datasource.DriverManagerDataSource;
import org.springframework.test.annotation.DirtiesContext;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.ownership.MintRequest;
import com.doodlearena.marketplace.ownership.MintService;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.PostgresTestConfiguration;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.marketplace.telemetry.MarketMetrics;

import static com.doodlearena.marketplace.ledger.Posting.credit;
import static com.doodlearena.marketplace.ledger.Posting.debit;
import static org.assertj.core.api.Assertions.assertThat;

/**
 * Breaks the ledger on purpose, so it gets a database of its own: the property gives this class its
 * own Spring context (and container), and the context is closed afterwards.
 */
@SpringBootTest(properties = "test.database=reconciliation")
@Import({ PostgresTestConfiguration.class, LedgerFixtures.class })
@DirtiesContext
class ReconciliationJobTests {

	@Autowired
	private ReconciliationJob job;

	@Autowired
	private LedgerService ledger;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private JdbcClient db;

	@Autowired
	private PlatformTransactionManager transactions;

	@Autowired
	private MarketMetrics metrics;

	@Autowired
	private MintService mints;

	@Autowired
	private LedgerMonths months;

	@Test
	void detectsWhatGetsPastTheDatabase() {
		AccountRef alice = this.fixtures.newUserWith(100);
		AccountRef bob = this.fixtures.newUser();
		this.ledger.post(TransactionType.SALE, "s:" + UUID.randomUUID(), Map.of(),
				List.of(debit(alice, 40), credit(bob, 40)));

		ReconciliationResult clean = this.job.reconcile();
		assertThat(clean.imbalance()).isZero();
		assertThat(clean.mismatchedAccountIds()).isEmpty();
		assertThat(this.job.current()).contains(clean);

		// What a disabled trigger or a hand-written statement could do: replica mode skips triggers
		// (the test user is a superuser), so an entry without its counterpart gets in.
		long bobAccount = accountId(bob);
		new TransactionTemplate(this.transactions).executeWithoutResult((status) -> {
			this.db.sql("SET LOCAL session_replication_role = replica").update();
			UUID tx = this.db.sql("INSERT INTO ledger_transactions (type, idempotency_key) VALUES ('sale', ?) RETURNING tx_id")
				.param("broken:" + UUID.randomUUID())
				.query(UUID.class)
				.single();
			this.db.sql("INSERT INTO ledger_entries (tx_id, account_id, amount) VALUES (?, ?, 7)")
				.params(tx, bobAccount)
				.update();
		});
		ReconciliationResult broken = this.job.reconcile();
		assertThat(broken.imbalance()).isEqualTo(7);
		assertThat(broken.mismatchedAccountIds()).containsExactly(bobAccount);

		// A balance edited by hand (balances aren't append-only; the CHECK still holds).
		long aliceAccount = accountId(alice);
		this.db.sql("UPDATE balances SET balance = balance + 3 WHERE account_id = ?").param(aliceAccount).update();
		assertThat(this.job.reconcile().mismatchedAccountIds()).containsExactly(aliceAccount, bobAccount);

		// A user account without a balance row.
		this.db.sql("INSERT INTO accounts (owner_type, owner_id) VALUES ('user', ?)").param("u_" + UUID.randomUUID()).update();
		assertThat(this.job.reconcile().mismatchedAccounts()).isEqualTo(3);
	}

	@Test
	void detectsOwnershipThatDisagreesWithItsProvenance() {
		UUID unsold = mint("u_ada");
		UUID saleNotApplied = mint("u_bob");
		UUID ownerEdited = mint("u_cyd");
		UUID authorsLost = mint("u_dee");
		assertThat(this.job.reconcile().ownershipViolationIds()).isEmpty();

		new TransactionTemplate(this.transactions).executeWithoutResult((status) -> {
			this.db.sql("SET LOCAL session_replication_role = replica").update();
			// A sale logged in the history but not applied to the ownership row.
			this.db.sql("""
					INSERT INTO ownership_events (drawing_id, kind, from_owner, to_owner, sale_id)
					VALUES (?, 'SALE', 'u_bob', 'u_eve', ?)""").params(saleNotApplied, UUID.randomUUID()).update();
			// History of a drawing that has no ownership row (the foreign key is bypassed here).
			this.db.sql("INSERT INTO ownership_events (drawing_id, kind, to_owner) VALUES (?, 'MINT', 'u_fay')")
				.param(UUID.randomUUID())
				.update();
		});
		this.db.sql("UPDATE ownership SET owner_id = 'u_mallory' WHERE drawing_id = ?").param(ownerEdited).update();
		this.db.sql("DELETE FROM drawing_authors WHERE drawing_id = ?").param(authorsLost).update();

		ReconciliationResult result = this.job.reconcile();
		assertThat(result.ownershipViolationIds()).contains(saleNotApplied, ownerEdited, authorsLost)
			.doesNotContain(unsold)
			.hasSize(4);
		assertThat(result.clean()).isFalse();
	}

	private UUID mint(String owner) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, owner)));
		return drawing;
	}

	@Test
	void staleResultsAreNotReported() {
		MutableClock clock = new MutableClock(Instant.parse("2026-10-18T12:00:00Z"));
		ReconciliationJob job = new ReconciliationJob(this.db, this.transactions, clock, this.metrics, this.months,
				Duration.ofMinutes(3));
		assertThat(job.current()).isEmpty();

		job.reconcile();
		clock.advance(Duration.ofMinutes(3));
		assertThat(job.current()).isPresent();
		clock.advance(Duration.ofSeconds(1));
		assertThat(job.current()).isEmpty();
	}

	@Test
	void scheduledRunReportsItsResult() {
		this.job.run();
		assertThat(this.job.current()).isPresent();
	}

	@Test
	void failedRunDoesNotThrowOrReport() {
		DataSource down = new DriverManagerDataSource("jdbc:postgresql://127.0.0.1:1/nowhere");
		ReconciliationJob job = new ReconciliationJob(JdbcClient.create(down), new DataSourceTransactionManager(down),
				Clock.systemUTC(), this.metrics, new LedgerMonths(JdbcClient.create(down), new DataSourceTransactionManager(down)),
				Duration.ofMinutes(3));

		job.run();

		assertThat(job.current()).isEmpty();
	}

	/**
	 * Task 6.6: months that ended count with their stored totals, the open ones are scanned, and the
	 * result is what a scan of the whole ledger gives. A change to a closed month's entries behind
	 * the triggers' back is still caught, by the rotating check of closed months.
	 */
	@Test
	void closedMonthsCountWithTheirTotalsAndAreCheckedAgain() {
		// A coin each now (an account exists from its first posting), then backdated rewards.
		AccountRef ada = this.fixtures.newUserWith(1);
		AccountRef bo = this.fixtures.newUserWith(1);
		long adaAccount = accountId(ada);
		long boAccount = accountId(bo);
		long issuer = this.db.sql("SELECT account_id FROM accounts WHERE owner_type = 'system' AND owner_id = 'rewards_issuance'")
			.query(Long.class)
			.single();
		Instant now = Instant.now();
		// Rewards paid 5 and 3 months ago, and a sale this month (through the service).
		pastPosting(now.minus(Duration.ofDays(150)), issuer, adaAccount, 50);
		pastPosting(now.minus(Duration.ofDays(90)), issuer, boAccount, 20);
		this.ledger.post(TransactionType.SALE, "s:" + UUID.randomUUID(), Map.of(), List.of(debit(ada, 10), credit(bo, 10)));

		// The same numbers as a scan of the whole ledger (other tests here break it on purpose, so
		// not necessarily 0), and nothing wrong with these two accounts.
		ReconciliationResult result = this.job.reconcile();
		assertThat(result.imbalance()).isEqualTo(fullImbalance());
		assertThat(result.mismatchedAccountIds()).isEqualTo(fullMismatched()).doesNotContain(adaAccount, boAccount);
		long closed = this.db.sql("SELECT count(*) FROM ledger_months_closed").query(Long.class).single();
		assertThat(closed).isGreaterThanOrEqualTo(3);
		// Stored totals plus the open months equal a scan of the whole ledger.
		Long stored = this.db.sql("SELECT SUM(total) FROM ledger_month_totals WHERE account_id = ?").param(adaAccount)
			.query(Long.class)
			.single();
		Long live = this.db.sql("""
				SELECT SUM(amount) FROM ledger_entries
				WHERE account_id = ? AND created_at < (SELECT max(month) + interval '1 month' FROM ledger_months_closed)""")
			.param(adaAccount)
			.query(Long.class)
			.single();
		assertThat(stored).isEqualTo(live).isEqualTo(50);

		// Someone changes the old reward behind the triggers' back (replica mode skips them).
		new TransactionTemplate(this.transactions).executeWithoutResult((status) -> {
			this.db.sql("SET LOCAL session_replication_role = replica").update();
			this.db.sql("UPDATE ledger_entries SET amount = 70 WHERE account_id = ? AND amount = 50").param(adaAccount).update();
		});
		// Each run checks the closed month checked longest ago: within as many runs as there are
		// closed months, the changed one is checked.
		long before = result.imbalance();
		boolean caught = false;
		for (int i = 0; i <= closed && !caught; i++) {
			ReconciliationResult run = this.job.reconcile();
			caught = run.imbalance() == before + 20 && run.mismatchedAccountIds().contains(adaAccount);
		}
		assertThat(caught).as("the changed closed month was reported").isTrue();
	}

	@Test
	void partitionsAreKeptAheadAndTheDefaultStaysEmpty() {
		this.months.ensurePartitions();
		List<String> partitions = this.db.sql("""
				SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
				WHERE i.inhparent = 'ledger_entries'::regclass ORDER BY 1""")
			.query(String.class)
			.list();
		java.time.YearMonth ahead = java.time.YearMonth.now(ZoneOffset.UTC).plusMonths(LedgerMonths.MONTHS_AHEAD);
		assertThat(partitions).contains("ledger_entries_default",
				"ledger_entries_" + ahead.toString().replace('-', '_'));
		assertThat(this.months.defaultPartitionRows()).isZero();
	}

	/** The imbalance a scan of the whole ledger finds, as the job computed it before task 6.6. */
	private long fullImbalance() {
		return this.db.sql("SELECT COALESCE(SUM(amount), 0) FROM ledger_entries").query(Long.class).single();
	}

	/** The mismatched accounts a scan of the whole ledger finds, as before task 6.6. */
	private List<Long> fullMismatched() {
		return this.db.sql("""
				SELECT a.account_id FROM accounts a LEFT JOIN balances b USING (account_id)
				LEFT JOIN (SELECT account_id, SUM(amount) AS total FROM ledger_entries GROUP BY account_id) e USING (account_id)
				WHERE b.account_id IS NULL OR b.balance <> COALESCE(e.total, 0) OR (a.owner_type = 'user' AND b.balance < 0)
				ORDER BY a.account_id""")
			.query(Long.class)
			.list();
	}

	/** A balanced posting at a past time, with its balances: what the service would have written then. */
	private void pastPosting(Instant at, long from, long to, long amount) {
		new TransactionTemplate(this.transactions).executeWithoutResult((status) -> {
			this.db.sql("SELECT ledger_ensure_partitions(CAST(? AS DATE), 0)").param(java.sql.Timestamp.from(at)).query().listOfRows();
			UUID tx = this.db.sql("""
					INSERT INTO ledger_transactions (type, idempotency_key, created_at) VALUES ('reward', ?, ?) RETURNING tx_id""")
				.params("past:" + UUID.randomUUID(), java.sql.Timestamp.from(at))
				.query(UUID.class)
				.single();
			this.db.sql("INSERT INTO ledger_entries (tx_id, account_id, amount, created_at) VALUES (?, ?, ?, ?), (?, ?, ?, ?)")
				.params(tx, from, -amount, java.sql.Timestamp.from(at), tx, to, amount, java.sql.Timestamp.from(at))
				.update();
			this.db.sql("UPDATE balances SET balance = balance - ? WHERE account_id = ?").params(amount, from).update();
			this.db.sql("UPDATE balances SET balance = balance + ? WHERE account_id = ?").params(amount, to).update();
		});
	}

	private long accountId(AccountRef user) {
		return this.db.sql("SELECT account_id FROM accounts WHERE owner_type = 'user' AND owner_id = ?")
			.param(user.ownerId())
			.query(Long.class)
			.single();
	}

	private static final class MutableClock extends Clock {

		private Instant now;

		MutableClock(Instant now) {
			this.now = now;
		}

		void advance(Duration duration) {
			this.now = this.now.plus(duration);
		}

		@Override
		public Instant instant() {
			return this.now;
		}

		@Override
		public ZoneId getZone() {
			return ZoneOffset.UTC;
		}

		@Override
		public Clock withZone(ZoneId zone) {
			return this;
		}

	}

}
