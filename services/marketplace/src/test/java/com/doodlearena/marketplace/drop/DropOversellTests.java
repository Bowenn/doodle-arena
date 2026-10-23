package com.doodlearena.marketplace.drop;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.Function;
import java.util.function.Supplier;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import org.junit.jupiter.api.Test;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.context.TestPropertySource;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.idempotency.IdempotentResponse;
import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.Posting;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.ownership.MintRequest;
import com.doodlearena.marketplace.ownership.MintService;
import com.doodlearena.marketplace.support.DropTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.TestEvents;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * The M6 criterion at test scale (ADR 0022): many buyers at once never get more editions than
 * exist, nobody pays without an edition, and the ledger balances. First with Redis telling the
 * truth, then with Redis wrong on purpose: Postgres's constraints still refuse every extra sale.
 * Also the measurement for the ADR: the Redis gate against a Postgres-only gate on the drop row.
 */
@TestPropertySource(properties = { "spring.datasource.hikari.maximum-pool-size=" + DropOversellTests.THREADS,
		"app.drops.rate-limit-burst=1000" })
class DropOversellTests extends DropTestSupport {

	private static final Logger log = LoggerFactory.getLogger(DropOversellTests.class);

	static final int THREADS = 32;

	static final int BUYERS = 300;

	static final int EDITIONS = 20;

	@Autowired
	private DropService drops;

	@Autowired
	private MintService mints;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private ReconciliationJob reconciliation;

	@Autowired
	private JdbcClient db;

	@Autowired
	private StringRedisTemplate redis;

	@Autowired
	private TransactionTemplate transactions;

	@Autowired
	private LedgerService ledger;

	@Test
	void manyBuyersAtOnceGetExactlyTheEditionsThatExist() throws Exception {
		Drop drop = openDrop();
		List<AccountRef> buyers = buyers(drop);

		Map<String, AtomicInteger> answers = buyAll(buyers, (buyer) -> answer(() -> this.drops
			.purchase(buyer.ownerId(), drop.dropId(), UUID.randomUUID().toString(), "h", "r")));

		log.atInfo().addKeyValue("answers", answers.toString()).log("oversell test, Redis honest");
		assertThat(answers.get("201").get()).isEqualTo(EDITIONS);
		assertSoldExactly(drop, buyers);
	}

	@Test
	void postgresRefusesWhatALyingRedisHandsOut() throws Exception {
		Drop drop = openDrop();
		List<AccountRef> buyers = buyers(drop);
		// Redis is wrong: every edition number three times over, so 3N reservations succeed.
		String free = DropGate.key(drop.dropId(), "free");
		for (int copy = 0; copy < 2; copy++) {
			for (int edition = 1; edition <= EDITIONS; edition++) {
				this.redis.opsForList().rightPush(free, Integer.toString(edition));
			}
		}

		Map<String, AtomicInteger> answers = buyAll(buyers, (buyer) -> answer(() -> this.drops
			.purchase(buyer.ownerId(), drop.dropId(), UUID.randomUUID().toString(), "h", "r")));

		log.atInfo().addKeyValue("answers", answers.toString()).log("oversell test, Redis lying");
		assertThat(answers.get("201").get()).isEqualTo(EDITIONS);
		assertThat(answers.getOrDefault("DROP_CONFLICT", new AtomicInteger()).get()).isPositive();
		assertSoldExactly(drop, buyers);

		// Redis forgets who bought (a flush): buyers who paid try again and are still refused.
		this.redis.delete(DropGate.key(drop.dropId(), "buyers"));
		this.redis.opsForList().rightPush(free, "1");
		Map<String, AtomicInteger> again = buyAll(buyers.subList(0, 40), (buyer) -> answer(() -> this.drops
			.purchase(buyer.ownerId(), drop.dropId(), UUID.randomUUID().toString(), "h", "r")));
		assertThat(again.containsKey("201")).isFalse();
		assertSoldExactly(drop, buyers);
		// Whatever Redis now marks as paid is exactly the edition Postgres holds for that buyer: a paid
		// buyer's retry gave its new reservation back and Redis relearned what they own.
		Map<Object, Object> entries = this.redis.opsForHash().entries(DropGate.key(drop.dropId(), "buyers"));
		entries.forEach((user, held) -> {
			if (String.valueOf(held).startsWith("p:")) {
				Integer owned = this.db.sql("SELECT edition_no FROM drop_editions WHERE drop_id = ? AND buyer_id = ?")
					.params(drop.dropId(), user)
					.query(Integer.class)
					.single();
				assertThat(held).isEqualTo("p:" + owned);
			}
		});
	}

	/**
	 * The ADR's measurement: the same burst through the Redis gate, and through a Postgres-only
	 * gate that locks the drop's row to count its editions. Timings are logged, not asserted.
	 */
	@Test
	void theRedisGateAgainstAPostgresOnlyCounter() throws Exception {
		Drop gated = openDrop();
		List<AccountRef> gatedBuyers = buyers(gated);
		long started = System.nanoTime();
		buyAll(gatedBuyers, (buyer) -> answer(() -> this.drops
			.purchase(buyer.ownerId(), gated.dropId(), UUID.randomUUID().toString(), "h", "r")));
		double gatedMs = (System.nanoTime() - started) / 1e6;

		Drop counted = openDrop();
		List<AccountRef> countedBuyers = new ArrayList<>();
		for (int i = 0; i < BUYERS; i++) {
			countedBuyers.add(this.fixtures.newUserWith(100));
		}
		started = System.nanoTime();
		Map<String, AtomicInteger> postgresOnly = buyAll(countedBuyers, (buyer) -> postgresOnly(counted, buyer));
		double countedMs = (System.nanoTime() - started) / 1e6;

		assertThat(postgresOnly.get("201").get()).isEqualTo(EDITIONS);
		log.atInfo()
			.addKeyValue("buyers", BUYERS)
			.addKeyValue("editions", EDITIONS)
			.addKeyValue("redis_gate_ms", Math.round(gatedMs))
			.addKeyValue("postgres_only_ms", Math.round(countedMs))
			.log("drop gate measured");
		System.out.printf("drop gate: %d buyers for %d editions on %d threads%n  Redis gate: %.0f ms%n  Postgres only: %.0f ms%n",
				BUYERS, EDITIONS, THREADS, gatedMs, countedMs);
	}

	/** A Postgres-only gate: every buyer locks the drop's row to count what is sold. */
	private String postgresOnly(Drop drop, AccountRef buyer) {
		String status = this.transactions.execute((tx) -> {
			// Lock first, count in the next statement: under READ COMMITTED a statement that waited for
			// the lock still counts with the snapshot it took before waiting, so a lock and a count in one
			// statement let two buyers see the same count (the first version of this test did).
			this.db.sql("SELECT 1 FROM drops WHERE drop_id = ? FOR UPDATE").param(drop.dropId()).query(Integer.class).single();
			int sold = this.db.sql("SELECT ? - COUNT(*) FROM drop_editions WHERE drop_id = ?")
				.params(drop.editions(), drop.dropId())
				.query(Integer.class)
				.single();
			if (sold <= 0) {
				return "SOLD_OUT";
			}
			int edition = EDITIONS - sold + 1;
			UUID txId = this.ledger.post(TransactionType.DROP_PURCHASE, "pg-only:" + UUID.randomUUID(), Map.of(),
					List.of(Posting.debit(buyer, drop.price()), Posting.credit(AccountRef.dropEscrow(0), drop.price())))
				.txId();
			this.db.sql("INSERT INTO drop_editions (drop_id, edition_no, editions, price, buyer_id, shard, status, tx_id) "
					+ "VALUES (?, ?, ?, ?, ?, 0, 'PAID', ?)")
				.params(drop.dropId(), edition, drop.editions(), drop.price(), buyer.ownerId(), txId)
				.update();
			return "201";
		});
		return (status == null) ? "500" : status;
	}

	private void assertSoldExactly(Drop drop, List<AccountRef> buyers) {
		long rows = this.db.sql("SELECT COUNT(*) FROM drop_editions WHERE drop_id = ?").param(drop.dropId())
			.query(Long.class)
			.single();
		long paid = buyers.stream().filter((buyer) -> this.fixtures.balance(buyer) == 100 - drop.price()).count();
		long untouched = buyers.stream().filter((buyer) -> this.fixtures.balance(buyer) == 100).count();
		assertThat(rows).isEqualTo(EDITIONS);
		assertThat(paid).isEqualTo(EDITIONS);
		assertThat(untouched).isEqualTo(buyers.size() - EDITIONS);
		assertThat(this.reconciliation.reconcile().clean()).isTrue();
	}

	private Drop openDrop() {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, this.fixtures.newUser().ownerId())));
		// Everyone admitted at once: this is about the inventory, not the waiting room.
		return this.drops.insert("u_admin", drawing, EDITIONS, 30, Instant.now().minusMillis(500),
				Instant.now().plus(Duration.ofHours(1)), 10_000, Instant.now());
	}

	private List<AccountRef> buyers(Drop drop) {
		List<AccountRef> buyers = new ArrayList<>();
		for (int i = 0; i < BUYERS; i++) {
			AccountRef buyer = this.fixtures.newUserWith(100);
			this.drops.join(buyer.ownerId(), drop.dropId());
			buyers.add(buyer);
		}
		return buyers;
	}

	/** Every buyer tries once, all at once; returns the count of each answer (status or error code). */
	private Map<String, AtomicInteger> buyAll(List<AccountRef> buyers, Function<AccountRef, String> buy)
			throws Exception {
		Map<String, AtomicInteger> answers = new ConcurrentHashMap<>();
		ExecutorService pool = Executors.newFixedThreadPool(THREADS);
		try {
			List<Future<?>> done = new ArrayList<>();
			for (AccountRef buyer : buyers) {
				done.add(pool.submit(() -> answers.computeIfAbsent(buy.apply(buyer), (k) -> new AtomicInteger()).incrementAndGet()));
			}
			for (Future<?> f : done) {
				f.get();
			}
		}
		finally {
			pool.shutdown();
		}
		return answers;
	}

	private static final Pattern CODE = Pattern.compile("\"code\":\"([A-Z_]+)\"");

	/** "201" for a sale, else the refusal's error code, whether thrown or stored with the key. */
	private static String answer(Supplier<IdempotentResponse> call) {
		try {
			IdempotentResponse response = call.get();
			if (response.status() == 201) {
				return "201";
			}
			Matcher code = CODE.matcher(response.body());
			return code.find() ? code.group(1) : Integer.toString(response.status());
		}
		catch (ApiException ex) {
			return ex.code();
		}
	}

}
