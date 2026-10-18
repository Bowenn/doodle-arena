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
		ReconciliationJob job = new ReconciliationJob(this.db, this.transactions, clock, this.metrics,
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
				Clock.systemUTC(), this.metrics, Duration.ofMinutes(3));

		job.run();

		assertThat(job.current()).isEmpty();
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
