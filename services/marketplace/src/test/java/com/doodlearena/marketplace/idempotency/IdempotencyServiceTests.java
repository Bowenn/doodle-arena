package com.doodlearena.marketplace.idempotency;

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
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.simple.JdbcClient;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.support.IntegrationTest;
import com.doodlearena.marketplace.support.LedgerFixtures;

import static com.doodlearena.marketplace.ledger.Posting.credit;
import static com.doodlearena.marketplace.ledger.Posting.debit;
import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

/**
 * The work in these tests pays a user 10 coins from the rewards account, so "done once" shows up
 * as a balance of exactly 10.
 */
@IntegrationTest
class IdempotencyServiceTests {

	private static final String HASH = IdempotencyService.hash("POST", "/api/market/test", "{}");

	@Autowired
	private IdempotencyService idempotency;

	@Autowired
	private LedgerService ledger;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private JdbcClient db;

	@Test
	void firstRequestRunsAndAReplayReturnsTheSameAnswer() {
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();

		IdempotentResponse first = pay(user, key, HASH);
		IdempotentResponse second = pay(user, key, HASH);

		assertThat(first.status()).isEqualTo(201);
		assertThat(first.replayed()).isFalse();
		assertThat(second.replayed()).isTrue();
		assertThat(second.status()).isEqualTo(201);
		assertThat(second.body()).isEqualTo(first.body());
		assertThat(this.fixtures.balance(user)).isEqualTo(10);
	}

	@Test
	void sameKeyForAnotherRequestIsRefused() {
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();
		pay(user, key, HASH);

		assertThatThrownBy(() -> pay(user, key, IdempotencyService.hash("POST", "/api/market/test", "{\"x\":1}")))
			.isInstanceOfSatisfying(ApiException.class, (ex) -> assertThat(ex.code()).isEqualTo("IDEMPOTENCY_KEY_REUSED"));
		assertThat(this.fixtures.balance(user)).isEqualTo(10);
	}

	@Test
	void keysBelongToTheirUser() {
		AccountRef alice = this.fixtures.newUser();
		AccountRef bob = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();

		pay(alice, key, HASH);
		pay(bob, key, HASH);

		assertThat(this.fixtures.balance(alice)).isEqualTo(10);
		assertThat(this.fixtures.balance(bob)).isEqualTo(10);
	}

	@Test
	void missingOrMalformedKeyIsRefusedBeforeAnyWork() {
		AccountRef user = this.fixtures.newUser();
		for (String key : new String[] { null, "", "has space", "x".repeat(256) }) {
			assertThatThrownBy(() -> pay(user, key, HASH)).isInstanceOfSatisfying(ApiException.class,
					(ex) -> assertThat(ex.code()).isEqualTo("IDEMPOTENCY_KEY_REQUIRED"));
		}
		assertThat(this.fixtures.balance(user)).isZero();
	}

	@Test
	void claimWithinItsLeaseIsInProgress() {
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();
		insertClaim(user.ownerId(), key, false);

		assertThatThrownBy(() -> pay(user, key, HASH)).isInstanceOfSatisfying(ApiException.class,
				(ex) -> assertThat(ex.code()).isEqualTo("IDEMPOTENCY_KEY_IN_PROGRESS"));
		assertThat(this.fixtures.balance(user)).isZero();
	}

	@Test
	void expiredClaimIsTakenOver() {
		// What a request that crashed after claiming its key leaves behind.
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();
		insertClaim(user.ownerId(), key, true);

		IdempotentResponse response = pay(user, key, HASH);

		assertThat(response.status()).isEqualTo(201);
		assertThat(this.fixtures.balance(user)).isEqualTo(10);
		assertThat(status(user.ownerId(), key)).isEqualTo("DONE");
	}

	/**
	 * A slow request loses its lease while it works; another takes the key over and finishes. The
	 * slow one's completion matches no row, so its work rolls back: paid once, not twice.
	 */
	@Test
	void takeoverFencesOutTheSlowRequest() throws Exception {
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();
		CountDownLatch working = new CountDownLatch(1);
		CountDownLatch resume = new CountDownLatch(1);

		try (ExecutorService pool = Executors.newSingleThreadExecutor()) {
			Future<IdempotentResponse> slow = pool.submit(() -> this.idempotency.execute(user.ownerId(), key, "test",
					HASH, "req-slow", HttpStatus.CREATED, (claim) -> {
						Object body = reward(user);
						working.countDown();
						await(resume);
						return body;
					}));
			assertThat(working.await(30, TimeUnit.SECONDS)).isTrue();
			this.db.sql("UPDATE idempotency_keys SET locked_until = now() - interval '1 second' WHERE user_id = ? AND key = ?")
				.params(user.ownerId(), key)
				.update();

			IdempotentResponse takeover = pay(user, key, HASH);
			resume.countDown();

			assertThat(takeover.status()).isEqualTo(201);
			assertThatThrownBy(() -> slow.get(30, TimeUnit.SECONDS)).cause()
				.isInstanceOfSatisfying(ApiException.class,
						(ex) -> assertThat(ex.code()).isEqualTo("IDEMPOTENCY_KEY_IN_PROGRESS"));
		}
		assertThat(this.fixtures.balance(user)).isEqualTo(10);
		assertThat(pay(user, key, HASH).replayed()).isTrue();
	}

	@Test
	void businessOutcomeIsStoredAndReplayed() {
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();
		AtomicInteger runs = new AtomicInteger();

		IdempotentResponse first = this.idempotency.execute(user.ownerId(), key, "test", HASH, "req-1",
				HttpStatus.CREATED, (claim) -> {
					runs.incrementAndGet();
					reward(user);
					throw new ApiException(HttpStatus.CONFLICT, "LISTING_NOT_AVAILABLE", "sold");
				});
		IdempotentResponse replay = this.idempotency.execute(user.ownerId(), key, "test", HASH, "req-2",
				HttpStatus.CREATED, (claim) -> {
					runs.incrementAndGet();
					return Map.of();
				});

		assertThat(first.status()).isEqualTo(409);
		assertThat(first.body()).contains("LISTING_NOT_AVAILABLE").contains("req-1");
		assertThat(replay.replayed()).isTrue();
		assertThat(replay.body()).isEqualTo(first.body());
		assertThat(runs).hasValue(1);
		// The work's own writes rolled back with the business outcome.
		assertThat(this.fixtures.balance(user)).isZero();
	}

	@Test
	void unexpectedFailureFreesTheKey() {
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();

		assertThatThrownBy(() -> this.idempotency.execute(user.ownerId(), key, "test", HASH, "req-1", HttpStatus.CREATED,
				(claim) -> {
					reward(user);
					throw new IllegalStateException("boom");
				})).isInstanceOf(IllegalStateException.class);

		assertThat(status(user.ownerId(), key)).isNull();
		assertThat(this.fixtures.balance(user)).isZero();
		assertThat(pay(user, key, HASH).status()).isEqualTo(201);
		assertThat(this.fixtures.balance(user)).isEqualTo(10);
	}

	@Test
	void concurrentRequestsWithOneKeyRunOnce() throws Exception {
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();
		Queue<String> outcomes = new ConcurrentLinkedQueue<>();
		CountDownLatch start = new CountDownLatch(1);

		try (ExecutorService pool = Executors.newFixedThreadPool(20)) {
			List<Future<?>> futures = new ArrayList<>();
			for (int i = 0; i < 20; i++) {
				futures.add(pool.submit(() -> {
					start.await();
					try {
						IdempotentResponse response = pay(user, key, HASH);
						outcomes.add(response.replayed() ? "replayed" : "ran");
					}
					catch (ApiException ex) {
						outcomes.add(ex.code());
					}
					return null;
				}));
			}
			start.countDown();
			for (Future<?> future : futures) {
				future.get(1, TimeUnit.MINUTES);
			}
		}

		assertThat(outcomes).hasSize(20).containsOnlyOnce("ran");
		assertThat(outcomes).allMatch((o) -> o.equals("ran") || o.equals("replayed") || o.equals("IDEMPOTENCY_KEY_IN_PROGRESS"));
		assertThat(this.fixtures.balance(user)).isEqualTo(10);
	}

	@Test
	void oldKeysAreDeleted() {
		AccountRef user = this.fixtures.newUser();
		String key = UUID.randomUUID().toString();
		pay(user, key, HASH);
		this.db.sql("UPDATE idempotency_keys SET created_at = now() - interval '25 hours' WHERE user_id = ? AND key = ?")
			.params(user.ownerId(), key)
			.update();

		assertThat(this.idempotency.deleteExpiredKeys()).isPositive();
		assertThat(status(user.ownerId(), key)).isNull();
	}

	private IdempotentResponse pay(AccountRef user, String key, String hash) {
		return this.idempotency.execute(user.ownerId(), key, "test", hash, "req", HttpStatus.CREATED,
				(claim) -> reward(user));
	}

	private Object reward(AccountRef user) {
		var result = this.ledger.post(TransactionType.REWARD, "test:" + UUID.randomUUID(), Map.of(),
				List.of(debit(AccountRef.REWARDS_ISSUANCE, 10), credit(user, 10)));
		return Map.of("tx_id", result.txId().toString());
	}

	private void insertClaim(String userId, String key, boolean expired) {
		this.db.sql("""
				INSERT INTO idempotency_keys (user_id, key, operation, request_hash, status, claim_token, locked_until)
				VALUES (?, ?, 'test', ?, 'IN_PROGRESS', ?, now() + (CASE WHEN ? THEN interval '-1 second' ELSE interval '30 seconds' END))""")
			.params(userId, key, HASH, UUID.randomUUID(), expired)
			.update();
	}

	private String status(String userId, String key) {
		return this.db.sql("SELECT status FROM idempotency_keys WHERE user_id = ? AND key = ?")
			.params(userId, key)
			.query(String.class)
			.optional()
			.orElse(null);
	}

	private static void await(CountDownLatch latch) {
		try {
			latch.await(30, TimeUnit.SECONDS);
		}
		catch (InterruptedException ex) {
			Thread.currentThread().interrupt();
		}
	}

}
