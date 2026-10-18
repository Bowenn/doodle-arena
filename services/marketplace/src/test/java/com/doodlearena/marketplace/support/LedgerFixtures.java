package com.doodlearena.marketplace.support;

import java.util.List;
import java.util.Map;
import java.util.UUID;

import org.springframework.boot.test.context.TestComponent;
import org.springframework.jdbc.core.simple.JdbcClient;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.TransactionType;

import static com.doodlearena.marketplace.ledger.Posting.credit;
import static com.doodlearena.marketplace.ledger.Posting.debit;

/** Users with fresh ids (tests share one database) and read-backs straight from the tables. */
@TestComponent
public class LedgerFixtures {

	private final JdbcClient db;

	private final LedgerService ledger;

	public LedgerFixtures(JdbcClient db, LedgerService ledger) {
		this.db = db;
		this.ledger = ledger;
	}

	public AccountRef newUser() {
		return AccountRef.user("u_" + UUID.randomUUID());
	}

	/** A user funded from the rewards account, as a match reward would. */
	public AccountRef newUserWith(long coins) {
		AccountRef user = newUser();
		fund(user, coins);
		return user;
	}

	public void fund(AccountRef user, long coins) {
		this.ledger.post(TransactionType.REWARD, "fixture:" + UUID.randomUUID(), Map.of(),
				List.of(debit(AccountRef.REWARDS_ISSUANCE, coins), credit(user, coins)));
	}

	public long balance(AccountRef account) {
		return this.db.sql("""
				SELECT b.balance FROM balances b JOIN accounts a USING (account_id, owner_type)
				WHERE a.owner_type = ? AND a.owner_id = ?""")
			.params(account.ownerType(), account.ownerId())
			.query(Long.class)
			.optional()
			.orElse(0L);
	}

	/** The sum of the account's entries: what its balance must equal. */
	public long entrySum(AccountRef account) {
		return this.db.sql("""
				SELECT COALESCE(SUM(e.amount), 0) FROM ledger_entries e JOIN accounts a USING (account_id)
				WHERE a.owner_type = ? AND a.owner_id = ?""")
			.params(account.ownerType(), account.ownerId())
			.query(Long.class)
			.single();
	}

	public List<Long> entries(UUID txId) {
		return this.db.sql("SELECT amount FROM ledger_entries WHERE tx_id = ?").param(txId).query(Long.class).list();
	}

	public String metadata(UUID txId) {
		return this.db.sql("SELECT metadata::text FROM ledger_transactions WHERE tx_id = ?")
			.param(txId)
			.query(String.class)
			.single();
	}

	public long transactionsWithKey(String key) {
		return this.db.sql("SELECT COUNT(*) FROM ledger_transactions WHERE idempotency_key = ?")
			.param(key)
			.query(Long.class)
			.single();
	}

}
