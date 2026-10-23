package com.doodlearena.marketplace.drop;

import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.jdbc.core.simple.JdbcClient;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.ledger.ReconciliationResult;
import com.doodlearena.marketplace.ownership.MintRequest;
import com.doodlearena.marketplace.ownership.MintService;
import com.doodlearena.marketplace.support.DropTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.proto.doodle.events.v1.CreditReason;
import com.doodlearena.proto.doodle.events.v1.WalletEvent;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * A drop's coins out of escrow to the drawing's authors, the sweeper's repairs, and the escrow
 * invariant (ADR 0022).
 */
class DropSettlementTests extends DropTestSupport {

	@Autowired
	private DropService drops;

	@Autowired
	private DropSettlement settlement;

	@Autowired
	private DropGate gate;

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

	@Test
	void aSoldOutDropPaysItsAuthorsOnceSplitLikeRoyalties() {
		AccountRef first = this.fixtures.newUser();
		AccountRef second = this.fixtures.newUser();
		Drop drop = openDrop(3, 7, first, second);
		for (int i = 0; i < 3; i++) {
			buy(drop, this.fixtures.newUserWith(10));
		}
		// Before the payout the authors have nothing; the escrow holds 21 coins.
		assertThat(this.fixtures.balance(first)).isZero();

		this.settlement.sweep();

		assertThat(this.fixtures.balance(first)).isEqualTo(11);
		assertThat(this.fixtures.balance(second)).isEqualTo(10);
		assertThat(status(drop)).containsOnlyKeys("SETTLED");
		assertThat(credits(first.ownerId())).containsExactly(CreditReason.CREDIT_REASON_DROP_SALE);
		this.settlement.sweep();
		assertThat(this.fixtures.balance(first)).isEqualTo(11);
		assertThat(this.fixtures.transactionsWithKey("drop-settle:" + drop.dropId() + ":1")).isOne();
		assertThat(this.fixtures.transactionsWithKey("drop-settle:" + drop.dropId() + ":2")).isZero();
		assertThat(settled(drop)).isTrue();
		assertClean();
	}

	@Test
	void aClosedDropPaysWhatItSoldAndAnOpenOneWaits() {
		AccountRef author = this.fixtures.newUser();
		Drop drop = openDrop(5, 4, author);
		buy(drop, this.fixtures.newUserWith(10));
		buy(drop, this.fixtures.newUserWith(10));

		this.settlement.sweep();
		assertThat(this.fixtures.balance(author)).isZero();

		this.db.sql("UPDATE drops SET opens_at = now() - interval '3 minutes', closes_at = now() - interval '2 minutes' WHERE drop_id = ?").param(drop.dropId()).update();
		this.settlement.sweep();
		// Closed for two minutes, past the minute of grace.
		assertThat(this.fixtures.balance(author)).isEqualTo(8);
		assertThat(settled(drop)).isTrue();
		assertClean();
	}

	@Test
	void aLeakedReservationIsGivenBackAndALostInventoryRebuilt() {
		Drop drop = openDrop(3, 4, this.fixtures.newUser());
		AccountRef buyer = this.fixtures.newUserWith(10);
		buy(drop, buyer);
		// A purchase that died between Redis and Postgres, two minutes ago.
		this.drops.join("u_ghost", drop.dropId());
		this.gate.reserve(drop, "u_ghost", Instant.now());
		this.redis.opsForHash().put(DropGate.key(drop.dropId(), "buyers"), "u_ghost",
				"r:" + ghostEdition(drop) + ":" + Instant.now().minus(Duration.ofMinutes(2)).toEpochMilli());
		assertThat(this.gate.editionsLeft(drop)).isEqualTo(1);

		this.settlement.sweep();
		assertThat(this.gate.editionsLeft(drop)).isEqualTo(2);

		// Redis loses the drop's inventory: rebuilt from Postgres's sales.
		this.redis.delete(List.of(DropGate.key(drop.dropId(), "free"), DropGate.key(drop.dropId(), "stocked")));
		this.settlement.sweep();
		assertThat(this.gate.editionsLeft(drop)).isEqualTo(2);
	}

	@Test
	void aShardThatDoesntHoldItsPaidEditionsIsReported() {
		Drop drop = openDrop(2, 9, this.fixtures.newUser());
		Edition edition = buy(drop, this.fixtures.newUserWith(10));
		int shard = (edition.editionNo() - 1) % AccountRef.DROP_ESCROW_SHARDS;
		this.db.sql("UPDATE drop_editions SET status = 'SETTLED', settle_tx_id = tx_id, settled_at = now() "
				+ "WHERE drop_id = ? AND edition_no = ?").params(drop.dropId(), edition.editionNo()).update();

		ReconciliationResult result = this.reconciliation.reconcile();
		assertThat(result.dropEscrowMismatchedShards()).containsExactly(shard);
		assertThat(result.clean()).isFalse();

		// The tests share a database: put it back.
		this.db.sql("UPDATE drop_editions SET status = 'PAID', settle_tx_id = NULL, settled_at = NULL "
				+ "WHERE drop_id = ? AND edition_no = ?").params(drop.dropId(), edition.editionNo()).update();
		assertClean();
	}

	@Test
	void sharesAreWholeCoinsAndAShareOfZeroIsLeftOut() {
		assertThat(DropSettlement.split(21, List.of("a", "b"))).containsExactly(Map.entry("a", 11L), Map.entry("b", 10L));
		assertThat(DropSettlement.split(1, List.of("a", "b"))).containsExactly(Map.entry("a", 1L));
	}

	private Drop openDrop(int editions, long price, AccountRef... authors) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing,
				java.util.Arrays.stream(authors).map(AccountRef::ownerId).toArray(String[]::new))));
		return this.drops.insert("u_admin", drawing, editions, price, Instant.now().minusMillis(500),
				Instant.now().plus(Duration.ofHours(1)), 1000, Instant.now());
	}

	private Edition buy(Drop drop, AccountRef buyer) {
		this.drops.join(buyer.ownerId(), drop.dropId());
		this.drops.purchase(buyer.ownerId(), drop.dropId(), UUID.randomUUID().toString(), "h", "r");
		return this.drops.editions(buyer.ownerId(), 1).getFirst();
	}

	private int ghostEdition(Drop drop) {
		String held = (String) this.redis.opsForHash().get(DropGate.key(drop.dropId(), "buyers"), "u_ghost");
		return Integer.parseInt(held.split(":")[1]);
	}

	private Map<String, Long> status(Drop drop) {
		return this.db.sql("SELECT status, COUNT(*) FROM drop_editions WHERE drop_id = ? GROUP BY status")
			.param(drop.dropId())
			.query((rs, i) -> Map.entry(rs.getString(1), rs.getLong(2)))
			.list()
			.stream()
			.collect(java.util.stream.Collectors.toMap(Map.Entry::getKey, Map.Entry::getValue));
	}

	private boolean settled(Drop drop) {
		return this.db.sql("SELECT settled_at IS NOT NULL FROM drops WHERE drop_id = ?").param(drop.dropId())
			.query(Boolean.class)
			.single();
	}

	private List<CreditReason> credits(String userId) {
		return this.db.sql("SELECT payload FROM outbox WHERE topic = 'market.wallet.v1' AND key = ? ORDER BY created_at")
			.param(userId)
			.query((rs, i) -> {
				try {
					return WalletEvent.parseFrom(rs.getBytes(1)).getCoinsCredited().getReason();
				}
				catch (com.google.protobuf.InvalidProtocolBufferException ex) {
					throw new IllegalStateException(ex);
				}
			})
			.list();
	}

	private void assertClean() {
		ReconciliationResult result = this.reconciliation.reconcile();
		assertThat(result.dropEscrowMismatchedShards()).isEmpty();
		assertThat(result.clean()).isTrue();
	}

}
