package com.doodlearena.marketplace.ledger;

import java.sql.Connection;
import java.sql.SQLException;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Queue;
import java.util.UUID;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import javax.sql.DataSource;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;

import com.doodlearena.marketplace.support.IntegrationTest;
import com.doodlearena.marketplace.support.LedgerFixtures;

import static com.doodlearena.marketplace.ledger.Posting.credit;
import static com.doodlearena.marketplace.ledger.Posting.debit;
import static org.assertj.core.api.Assertions.assertThat;

/** The ledger under concurrent postings: no deadlock, no overdraft, no double effect. */
@IntegrationTest
class LedgerConcurrencyTests {

	private static final int THREADS = 8;

	@Autowired
	private LedgerService ledger;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private DataSource dataSource;

	/** M4 exit criterion: concurrent transfers in opposite directions finish without a deadlock. */
	@Test
	void oppositeTransfersDoNotDeadlock() throws Exception {
		AccountRef alice = this.fixtures.newUserWith(1_000);
		AccountRef bob = this.fixtures.newUserWith(1_000);
		int perThread = 100;

		Queue<Throwable> failures = runConcurrently(THREADS, (thread) -> {
			boolean aliceSends = thread % 2 == 0;
			for (int i = 0; i < perThread; i++) {
				AccountRef from = aliceSends ? alice : bob;
				AccountRef to = aliceSends ? bob : alice;
				this.ledger.post(TransactionType.SALE, "t:" + UUID.randomUUID(), Map.of(),
						List.of(debit(from, 1), credit(to, 1)));
			}
		});

		assertThat(failures).isEmpty();
		// Half the threads moved 400 one way, half 400 the other.
		assertThat(this.fixtures.balance(alice)).isEqualTo(1_000);
		assertThat(this.fixtures.balance(bob)).isEqualTo(1_000);
		assertThat(this.fixtures.entrySum(alice)).isEqualTo(1_000);
		assertThat(this.fixtures.entrySum(bob)).isEqualTo(1_000);
	}

	/** Random three-legged transactions over a small set of accounts: lots of lock overlap. */
	@Test
	void randomTransfersKeepEveryBalanceEqualToItsEntries() throws Exception {
		List<AccountRef> accounts = new ArrayList<>();
		for (int i = 0; i < 10; i++) {
			accounts.add(this.fixtures.newUserWith(500));
		}
		AtomicInteger refused = new AtomicInteger();

		Queue<Throwable> failures = runConcurrently(THREADS, (thread) -> {
			ThreadLocalRandom random = ThreadLocalRandom.current();
			for (int i = 0; i < 100; i++) {
				AccountRef payer = accounts.get(random.nextInt(accounts.size()));
				AccountRef first = accounts.get(random.nextInt(accounts.size()));
				AccountRef second = accounts.get(random.nextInt(accounts.size()));
				long a = 1 + random.nextInt(40);
				long b = 1 + random.nextInt(40);
				try {
					this.ledger.post(TransactionType.SALE, "r:" + UUID.randomUUID(), Map.of(),
							List.of(debit(payer, a + b), credit(first, a), credit(second, b)));
				}
				catch (InsufficientFundsException ex) {
					refused.incrementAndGet();
				}
				catch (IllegalArgumentException ex) {
					// The payer drew itself twice: the postings cancel out. Not under test here.
				}
			}
		});

		assertThat(failures).isEmpty();
		long total = 0;
		for (AccountRef account : accounts) {
			long balance = this.fixtures.balance(account);
			assertThat(balance).isNotNegative().isEqualTo(this.fixtures.entrySum(account));
			total += balance;
		}
		// Coins only moved between these ten accounts.
		assertThat(total).isEqualTo(5_000);
	}

	@Test
	void sameKeyFromManyThreadsPostsOnce() throws Exception {
		AccountRef user = this.fixtures.newUser();
		String key = "reward:race:" + user.ownerId();
		AtomicInteger fresh = new AtomicInteger();
		Queue<UUID> ids = new ConcurrentLinkedQueue<>();

		Queue<Throwable> failures = runConcurrently(20, (thread) -> {
			PostResult result = this.ledger.post(TransactionType.REWARD, key, Map.of(),
					List.of(debit(AccountRef.REWARDS_ISSUANCE, 10), credit(user, 10)));
			ids.add(result.txId());
			if (!result.replayed()) {
				fresh.incrementAndGet();
			}
		});

		assertThat(failures).isEmpty();
		assertThat(fresh).hasValue(1);
		assertThat(ids).hasSize(20).containsOnly(ids.peek());
		assertThat(this.fixtures.balance(user)).isEqualTo(10);
	}

	@Test
	void concurrentSpendingNeverOverdraws() throws Exception {
		AccountRef payer = this.fixtures.newUserWith(50);
		AtomicInteger succeeded = new AtomicInteger();
		AtomicInteger refused = new AtomicInteger();

		Queue<Throwable> failures = runConcurrently(THREADS, (thread) -> {
			for (int i = 0; i < 100 / THREADS + 1; i++) {
				try {
					this.ledger.post(TransactionType.SALE, "s:" + UUID.randomUUID(), Map.of(),
							List.of(debit(payer, 1), credit(this.fixtures.newUser(), 1)));
					succeeded.incrementAndGet();
				}
				catch (InsufficientFundsException ex) {
					refused.incrementAndGet();
				}
			}
		});

		assertThat(failures).isEmpty();
		assertThat(succeeded).hasValue(50);
		assertThat(refused.get()).isEqualTo(THREADS * (100 / THREADS + 1) - 50);
		assertThat(this.fixtures.balance(payer)).isZero();
	}

	/**
	 * Why the lock order matters: two transactions that lock the same two balance rows one at a
	 * time, in opposite orders, deadlock, and Postgres aborts one of them (SQLSTATE 40P01). The
	 * service avoids this by locking all rows in one statement sorted by account_id.
	 */
	@Test
	void lockingInRequestOrderDeadlocks() throws Exception {
		AccountRef alice = this.fixtures.newUserWith(10);
		AccountRef bob = this.fixtures.newUserWith(10);
		long a = accountId(alice);
		long b = accountId(bob);
		CountDownLatch bothHoldFirstLock = new CountDownLatch(2);
		Queue<String> sqlStates = new ConcurrentLinkedQueue<>();

		Queue<Throwable> failures = runConcurrently(2, (thread) -> {
			long first = (thread == 0) ? a : b;
			long second = (thread == 0) ? b : a;
			try (Connection conn = this.dataSource.getConnection()) {
				conn.setAutoCommit(false);
				try {
					lock(conn, first);
					bothHoldFirstLock.countDown();
					bothHoldFirstLock.await(10, TimeUnit.SECONDS);
					lock(conn, second);
				}
				catch (SQLException ex) {
					sqlStates.add(ex.getSQLState());
				}
				finally {
					conn.rollback();
				}
			}
		});

		assertThat(failures).isEmpty();
		assertThat(sqlStates).containsExactly("40P01");
	}

	private long accountId(AccountRef user) throws SQLException {
		try (Connection conn = this.dataSource.getConnection();
				var statement = conn.prepareStatement("SELECT account_id FROM accounts WHERE owner_type = 'user' AND owner_id = ?")) {
			statement.setString(1, user.ownerId());
			try (var rs = statement.executeQuery()) {
				rs.next();
				return rs.getLong(1);
			}
		}
	}

	private static void lock(Connection conn, long accountId) throws SQLException {
		try (var statement = conn.prepareStatement("SELECT balance FROM balances WHERE account_id = ? FOR UPDATE")) {
			statement.setLong(1, accountId);
			statement.executeQuery().close();
		}
	}

	/** Runs the body on n threads released at the same moment; returns what they threw. */
	private static Queue<Throwable> runConcurrently(int threads, ThreadBody body) throws Exception {
		Queue<Throwable> failures = new ConcurrentLinkedQueue<>();
		CountDownLatch start = new CountDownLatch(1);
		try (ExecutorService pool = Executors.newFixedThreadPool(threads)) {
			List<Future<?>> futures = new ArrayList<>();
			for (int t = 0; t < threads; t++) {
				int thread = t;
				futures.add(pool.submit(() -> {
					try {
						start.await();
						body.run(thread);
					}
					catch (Throwable ex) {
						failures.add(ex);
					}
				}));
			}
			start.countDown();
			for (Future<?> future : futures) {
				future.get(2, TimeUnit.MINUTES);
			}
		}
		return failures;
	}

	@FunctionalInterface
	private interface ThreadBody {

		void run(int thread) throws Exception;

	}

}
