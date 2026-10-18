package com.doodlearena.marketplace.ledger;

import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.dao.DataAccessException;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.support.IntegrationTest;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

/** The rules Postgres enforces by itself, whatever the service does (migration V1). */
@IntegrationTest
class LedgerSchemaTests {

	@Autowired
	private JdbcClient db;

	@Autowired
	private TransactionTemplate tx;

	@Test
	void systemAccountsExist() {
		var names = this.db.sql("SELECT owner_id FROM accounts JOIN balances USING (account_id, owner_type) "
				+ "WHERE owner_type = 'system' ORDER BY owner_id")
			.query(String.class)
			.list();
		assertThat(names).containsExactly("rewards_issuance", "stripe_clearing");
	}

	@Test
	void balancedTransactionCommits() {
		long a = issuance();
		long b = userAccount();
		UUID id = this.tx.execute((status) -> {
			UUID created = newTransaction();
			entry(created, a, -5);
			entry(created, b, 5);
			return created;
		});
		assertThat(this.db.sql("SELECT COUNT(*) FROM ledger_entries WHERE tx_id = ?").param(id).query(Long.class).single())
			.isEqualTo(2);
	}

	@Test
	void unbalancedTransactionFailsAtCommit() {
		long a = issuance();
		long b = userAccount();
		assertThatThrownBy(() -> this.tx.executeWithoutResult((status) -> {
			UUID id = newTransaction();
			entry(id, a, -5);
			entry(id, b, 4);
		})).isInstanceOf(DataAccessException.class).hasMessageContaining("unbalanced");
		assertThat(entriesOf(b)).isZero();
	}

	@Test
	void transactionWithOneEntryFails() {
		long a = userAccount();
		assertThatThrownBy(() -> this.tx.executeWithoutResult((status) -> {
			UUID id = newTransaction();
			entry(id, a, 7);
		})).hasMessageContaining("unbalanced");
	}

	@Test
	void transactionWithoutEntriesFails() {
		assertThatThrownBy(() -> this.tx.executeWithoutResult((status) -> newTransaction()))
			.hasMessageContaining("unbalanced");
	}

	@Test
	void zeroAmountEntryIsRefused() {
		long a = userAccount();
		assertThatThrownBy(() -> this.tx.executeWithoutResult((status) -> {
			UUID id = newTransaction();
			entry(id, a, 0);
		})).isInstanceOf(DataAccessException.class);
	}

	@Test
	void ledgerRowsCannotBeChangedOrRemoved() {
		long a = issuance();
		long b = userAccount();
		UUID id = this.tx.execute((status) -> {
			UUID created = newTransaction();
			entry(created, a, -3);
			entry(created, b, 3);
			return created;
		});
		assertThatThrownBy(() -> this.db.sql("UPDATE ledger_entries SET amount = amount * 2 WHERE tx_id = ?")
			.param(id)
			.update()).hasMessageContaining("append-only");
		assertThatThrownBy(() -> this.db.sql("DELETE FROM ledger_entries WHERE tx_id = ?").param(id).update())
			.hasMessageContaining("append-only");
		assertThatThrownBy(() -> this.db.sql("UPDATE ledger_transactions SET type = 'sale' WHERE tx_id = ?")
			.param(id)
			.update()).hasMessageContaining("append-only");
		assertThatThrownBy(() -> this.db.sql("DELETE FROM ledger_transactions WHERE tx_id = ?").param(id).update())
			.hasMessageContaining("append-only");
		assertThatThrownBy(() -> this.db.sql("TRUNCATE ledger_entries, ledger_transactions CASCADE").update())
			.hasMessageContaining("append-only");
	}

	@Test
	void userBalanceCannotGoNegative() {
		long a = userAccount();
		assertThatThrownBy(() -> this.db.sql("UPDATE balances SET balance = -1 WHERE account_id = ?")
			.param(a)
			.update()).hasMessageContaining("balances_user_not_negative");
	}

	@Test
	void systemBalanceMayGoNegative() {
		long issuance = this.db
			.sql("SELECT account_id FROM accounts WHERE owner_type = 'system' AND owner_id = 'rewards_issuance'")
			.query(Long.class)
			.single();
		this.tx.executeWithoutResult((status) -> {
			this.db.sql("UPDATE balances SET balance = balance - 1 WHERE account_id = ?").param(issuance).update();
			status.setRollbackOnly();
		});
	}

	@Test
	void balanceOwnerTypeMustMatchItsAccount() {
		// A user account can't get a balance row that claims to be a system account (and may go
		// negative). Rolled back, so no account without a balance row is left behind.
		assertThatThrownBy(() -> this.tx.executeWithoutResult((status) -> {
			long a = this.db.sql("INSERT INTO accounts (owner_type, owner_id) VALUES ('user', ?) RETURNING account_id")
				.param("u_" + UUID.randomUUID())
				.query(Long.class)
				.single();
			this.db.sql("INSERT INTO balances (account_id, owner_type) VALUES (?, 'system')").param(a).update();
		})).isInstanceOf(DataAccessException.class);
	}

	/** The rewards account: a system account, so it may be debited below zero. */
	private long issuance() {
		return this.db
			.sql("SELECT account_id FROM accounts WHERE owner_type = 'system' AND owner_id = 'rewards_issuance'")
			.query(Long.class)
			.single();
	}

	private long userAccount() {
		long id = this.db.sql("INSERT INTO accounts (owner_type, owner_id) VALUES ('user', ?) RETURNING account_id")
			.param("u_" + UUID.randomUUID())
			.query(Long.class)
			.single();
		this.db.sql("INSERT INTO balances (account_id, owner_type) VALUES (?, 'user')").param(id).update();
		return id;
	}

	private UUID newTransaction() {
		return this.db.sql("INSERT INTO ledger_transactions (type, idempotency_key) VALUES ('reward', ?) RETURNING tx_id")
			.param("test:" + UUID.randomUUID())
			.query(UUID.class)
			.single();
	}

	/**
	 * An entry plus the balance change that goes with it: other test classes share this database,
	 * and the reconciliation tests expect every balance to equal its entries.
	 */
	private void entry(UUID txId, long accountId, long amount) {
		this.db.sql("INSERT INTO ledger_entries (tx_id, account_id, amount) VALUES (?, ?, ?)")
			.params(txId, accountId, amount)
			.update();
		this.db.sql("UPDATE balances SET balance = balance + ? WHERE account_id = ?").params(amount, accountId).update();
	}

	private long entriesOf(long accountId) {
		return this.db.sql("SELECT COUNT(*) FROM ledger_entries WHERE account_id = ?")
			.param(accountId)
			.query(Long.class)
			.single();
	}

}
