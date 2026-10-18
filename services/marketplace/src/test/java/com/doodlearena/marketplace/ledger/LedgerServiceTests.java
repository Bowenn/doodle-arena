package com.doodlearena.marketplace.ledger;

import java.util.List;
import java.util.Map;
import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;

import com.doodlearena.marketplace.support.IntegrationTest;
import com.doodlearena.marketplace.support.LedgerFixtures;

import static com.doodlearena.marketplace.ledger.Posting.credit;
import static com.doodlearena.marketplace.ledger.Posting.debit;
import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

@IntegrationTest
class LedgerServiceTests {

	@Autowired
	private LedgerService ledger;

	@Autowired
	private LedgerFixtures fixtures;

	@Test
	void rewardCreditsUserAndDebitsIssuance() {
		AccountRef user = this.fixtures.newUser();
		long issuanceBefore = this.fixtures.balance(AccountRef.REWARDS_ISSUANCE);

		PostResult result = this.ledger.post(TransactionType.REWARD, "reward:m1:" + user.ownerId(),
				Map.of("match_id", "m1"), List.of(debit(AccountRef.REWARDS_ISSUANCE, 30), credit(user, 30)));

		assertThat(result.replayed()).isFalse();
		assertThat(this.fixtures.balance(user)).isEqualTo(30);
		// Other tests share the database, so check the issuance account's change, not its value.
		assertThat(this.fixtures.balance(AccountRef.REWARDS_ISSUANCE)).isLessThanOrEqualTo(issuanceBefore - 30);
		assertThat(this.fixtures.entries(result.txId())).containsExactlyInAnyOrder(-30L, 30L);
		assertThat(this.fixtures.metadata(result.txId())).isEqualTo("{\"match_id\": \"m1\"}");
	}

	@Test
	void replayChangesNothing() {
		AccountRef user = this.fixtures.newUser();
		String key = "reward:m2:" + user.ownerId();
		List<Posting> postings = List.of(debit(AccountRef.REWARDS_ISSUANCE, 20), credit(user, 20));

		PostResult first = this.ledger.post(TransactionType.REWARD, key, Map.of(), postings);
		PostResult second = this.ledger.post(TransactionType.REWARD, key, Map.of(), postings);

		assertThat(second.txId()).isEqualTo(first.txId());
		assertThat(second.replayed()).isTrue();
		assertThat(this.fixtures.balance(user)).isEqualTo(20);
		assertThat(this.fixtures.transactionsWithKey(key)).isEqualTo(1);
	}

	@Test
	void keyOfAnotherTypeIsAConflict() {
		AccountRef user = this.fixtures.newUser();
		String key = "k:" + user.ownerId();
		this.ledger.post(TransactionType.REWARD, key, Map.of(),
				List.of(debit(AccountRef.REWARDS_ISSUANCE, 5), credit(user, 5)));

		assertThatThrownBy(() -> this.ledger.post(TransactionType.STRIPE_PURCHASE, key, Map.of(),
				List.of(debit(AccountRef.STRIPE_CLEARING, 5), credit(user, 5))))
			.isInstanceOf(IdempotencyConflictException.class);
		assertThat(this.fixtures.balance(user)).isEqualTo(5);
	}

	@Test
	void insufficientFundsWritesNothing() {
		AccountRef alice = this.fixtures.newUserWith(10);
		AccountRef bob = this.fixtures.newUser();
		String key = "sale:" + UUID.randomUUID();

		assertThatThrownBy(() -> this.ledger.post(TransactionType.SALE, key, Map.of(),
				List.of(debit(alice, 11), credit(bob, 11))))
			.isInstanceOf(InsufficientFundsException.class);

		assertThat(this.fixtures.balance(alice)).isEqualTo(10);
		assertThat(this.fixtures.balance(bob)).isZero();
		// The transaction row rolled back too, so the same key can still succeed later.
		assertThat(this.fixtures.transactionsWithKey(key)).isZero();
		this.ledger.post(TransactionType.SALE, key, Map.of(), List.of(debit(alice, 10), credit(bob, 10)));
		assertThat(this.fixtures.balance(bob)).isEqualTo(10);
	}

	@Test
	void systemAccountsMayGoNegative() {
		AccountRef user = this.fixtures.newUser();
		long clearing = this.fixtures.balance(AccountRef.STRIPE_CLEARING);
		long amount = Math.max(clearing, 0) + 1_000;

		this.ledger.post(TransactionType.STRIPE_PURCHASE, "stripe:" + UUID.randomUUID(), Map.of(),
				List.of(debit(AccountRef.STRIPE_CLEARING, amount), credit(user, amount)));

		assertThat(this.fixtures.balance(AccountRef.STRIPE_CLEARING)).isNegative();
	}

	@Test
	void repeatedAccountIsMergedIntoOneEntry() {
		// A buyer who is also an author pays the price and gets a royalty share back.
		AccountRef buyer = this.fixtures.newUserWith(100);
		AccountRef seller = this.fixtures.newUser();

		PostResult result = this.ledger.post(TransactionType.SALE, "sale:" + UUID.randomUUID(), Map.of(),
				List.of(debit(buyer, 100), credit(seller, 90), credit(buyer, 10)));

		assertThat(this.fixtures.entries(result.txId())).containsExactlyInAnyOrder(-90L, 90L);
		assertThat(this.fixtures.balance(buyer)).isEqualTo(10);
		assertThat(this.fixtures.balance(seller)).isEqualTo(90);
	}

	@Test
	void unbalancedPostingsAreRefusedBeforeAnyWrite() {
		AccountRef a = this.fixtures.newUserWith(50);
		AccountRef b = this.fixtures.newUser();

		assertThatThrownBy(() -> this.ledger.post(TransactionType.SALE, "x:" + UUID.randomUUID(), Map.of(),
				List.of(debit(a, 5), credit(b, 4)))).isInstanceOf(IllegalArgumentException.class)
			.hasMessageContaining("sum to -1");
		assertThatThrownBy(() -> this.ledger.post(TransactionType.SALE, "x:" + UUID.randomUUID(), Map.of(),
				List.of(credit(b, 0), debit(a, 0)))).isInstanceOf(IllegalArgumentException.class);
		assertThatThrownBy(() -> this.ledger.post(TransactionType.SALE, "x:" + UUID.randomUUID(), Map.of(),
				List.of(credit(b, 5)))).isInstanceOf(IllegalArgumentException.class);
		assertThatThrownBy(() -> this.ledger.post(TransactionType.SALE, "x:" + UUID.randomUUID(), Map.of(),
				List.of(debit(a, 5), credit(a, 5)))).isInstanceOf(IllegalArgumentException.class)
			.hasMessageContaining("cancel out");
		assertThatThrownBy(() -> this.ledger.post(TransactionType.SALE, " ", Map.of(),
				List.of(debit(a, 5), credit(b, 5)))).isInstanceOf(IllegalArgumentException.class);
		assertThat(this.fixtures.balance(a)).isEqualTo(50);
	}

	@Test
	void unknownSystemAccountIsRefused() {
		AccountRef user = this.fixtures.newUser();
		assertThatThrownBy(() -> this.ledger.post(TransactionType.REWARD, "r:" + UUID.randomUUID(), Map.of(),
				List.of(debit(new AccountRef(AccountRef.SYSTEM, "nowhere"), 5), credit(user, 5))))
			.isInstanceOf(IllegalStateException.class);
	}

}
