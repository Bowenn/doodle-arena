package com.doodlearena.marketplace.ledger;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.UUID;
import java.util.stream.Collectors;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Isolation;
import org.springframework.transaction.annotation.Transactional;

import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * Posts double-entry transactions to the coin ledger (design doc, section 8). Every money-moving
 * feature (rewards, Stripe, purchases) goes through {@link #post}.
 *
 * <p>The rules, in the order they are checked:
 * <ol>
 * <li>The postings balance: at least two accounts, no zero amount, and the amounts sum to zero.
 * Postgres checks this again at COMMIT.</li>
 * <li>Each transaction has an idempotency key with a UNIQUE constraint. A second call with the same
 * key finds the first transaction and changes nothing.</li>
 * <li>Balance rows are locked in ascending {@code account_id} order, so two transactions that touch
 * the same accounts in opposite directions queue instead of deadlocking.</li>
 * <li>No user balance goes below zero (system accounts may). Postgres has the same CHECK.</li>
 * </ol>
 */
@Service
public class LedgerService {

	private static final Logger log = LoggerFactory.getLogger(LedgerService.class);

	private final JdbcClient db;

	private final JsonMapper json;

	private final MarketMetrics metrics;

	public LedgerService(JdbcClient db, JsonMapper json, MarketMetrics metrics) {
		this.db = db;
		this.json = json;
		this.metrics = metrics;
	}

	/**
	 * Writes one ledger transaction, or returns the existing one with the same idempotency key.
	 * Joins the caller's transaction when there is one (a purchase also moves ownership in it), so
	 * an exception rolls back everything, including the transaction row: a later call with the same
	 * key can then succeed.
	 * @throws IllegalArgumentException the postings don't balance
	 * @throws InsufficientFundsException a user balance would go below zero
	 * @throws IdempotencyConflictException the key belongs to a transaction of another type
	 */
	@Transactional(isolation = Isolation.READ_COMMITTED)
	public PostResult post(TransactionType type, String idempotencyKey, Map<String, String> metadata,
			List<Posting> postings) {
		if (idempotencyKey == null || idempotencyKey.isBlank()) {
			throw new IllegalArgumentException("idempotency key is required");
		}
		Map<AccountRef, Long> changes = netChanges(postings);

		// The UNIQUE key decides between a new transaction and a replay. A concurrent call with the
		// same key waits here until the first one commits (then sees its row) or rolls back.
		UUID txId = this.db.sql("""
				INSERT INTO ledger_transactions (type, idempotency_key, metadata)
				VALUES (?, ?, ?::jsonb)
				ON CONFLICT (idempotency_key) DO NOTHING
				RETURNING tx_id""")
			.params(type.dbValue(), idempotencyKey, this.json.writeValueAsString(metadata))
			.query(UUID.class)
			.optional()
			.orElse(null);
		if (txId == null) {
			return replay(type, idempotencyKey);
		}

		Map<Long, AccountRef> accounts = new TreeMap<>();
		for (AccountRef account : changes.keySet()) {
			accounts.put(accountId(account), account);
		}
		Map<Long, Long> balances = lockBalances(accounts.keySet());

		for (Map.Entry<Long, AccountRef> account : accounts.entrySet()) {
			long change = changes.get(account.getValue());
			long balance = balances.get(account.getKey());
			if (account.getValue().isUser() && balance + change < 0) {
				throw new InsufficientFundsException(account.getValue(), balance, change);
			}
		}
		for (Map.Entry<Long, AccountRef> account : accounts.entrySet()) {
			long change = changes.get(account.getValue());
			this.db.sql("INSERT INTO ledger_entries (tx_id, account_id, amount) VALUES (?, ?, ?)")
				.params(txId, account.getKey(), change)
				.update();
			this.db.sql("UPDATE balances SET balance = balance + ?, updated_at = now() WHERE account_id = ?")
				.params(change, account.getKey())
				.update();
		}
		log.atInfo()
			.addKeyValue("tx_id", txId)
			.addKeyValue("tx_type", type.dbValue())
			.addKeyValue("accounts", accounts.size())
			.log("ledger transaction posted");
		return new PostResult(txId, false);
	}

	/**
	 * Merges postings to the same account (a buyer who is also an author pays the price and gets a
	 * royalty share: one entry, one lock) and checks that they balance.
	 */
	static Map<AccountRef, Long> netChanges(List<Posting> postings) {
		if (postings == null || postings.size() < 2) {
			throw new IllegalArgumentException("a transaction needs at least two postings");
		}
		long sum = 0;
		for (Posting posting : postings) {
			if (posting.amount() == 0) {
				throw new IllegalArgumentException("a posting can't be zero");
			}
			sum = Math.addExact(sum, posting.amount());
		}
		if (sum != 0) {
			throw new IllegalArgumentException("postings sum to " + sum + ", not zero");
		}
		Map<AccountRef, Long> merged = postings.stream()
			.collect(Collectors.groupingBy(Posting::account, Collectors.summingLong(Posting::amount)));
		merged.values().removeIf((amount) -> amount == 0);
		if (merged.size() < 2) {
			throw new IllegalArgumentException("the postings cancel out: nothing would move");
		}
		return merged;
	}

	private PostResult replay(TransactionType type, String idempotencyKey) {
		record Existing(UUID txId, String type) {
		}
		Existing existing = this.db.sql("SELECT tx_id, type FROM ledger_transactions WHERE idempotency_key = ?")
			.param(idempotencyKey)
			.query((rs, row) -> new Existing(rs.getObject("tx_id", UUID.class), rs.getString("type")))
			.single();
		if (!existing.type().equals(type.dbValue())) {
			throw new IdempotencyConflictException(
					"idempotency key belongs to a " + existing.type() + " transaction, not " + type.dbValue());
		}
		log.atInfo()
			.addKeyValue("tx_id", existing.txId())
			.addKeyValue("tx_type", type.dbValue())
			.log("ledger transaction replayed");
		return new PostResult(existing.txId(), true);
	}

	/** The account's id; a user's account and balance row are created on their first posting. */
	private long accountId(AccountRef account) {
		if (account.isUser()) {
			// A concurrent first posting for the same user waits on the UNIQUE index, then sees the row.
			Long created = this.db.sql("""
					INSERT INTO accounts (owner_type, owner_id) VALUES ('user', ?)
					ON CONFLICT (owner_type, owner_id) DO NOTHING
					RETURNING account_id""")
				.param(account.ownerId())
				.query(Long.class)
				.optional()
				.orElse(null);
			if (created != null) {
				this.db.sql("INSERT INTO balances (account_id, owner_type) VALUES (?, 'user')").param(created).update();
				return created;
			}
		}
		return this.db.sql("SELECT account_id FROM accounts WHERE owner_type = ? AND owner_id = ?")
			.params(account.ownerType(), account.ownerId())
			.query(Long.class)
			.optional()
			.orElseThrow(() -> new IllegalStateException("no " + account.ownerType() + " account " + account.ownerId()));
	}

	/**
	 * Locks the balance rows in ascending account_id order and returns their balances. Postgres
	 * sorts before it locks (the LockRows step runs above the Sort), so every transaction takes the
	 * locks in the same order and none can hold one lock while waiting for another that a second
	 * transaction holds while waiting for the first.
	 */
	private Map<Long, Long> lockBalances(Set<Long> accountIds) {
		long started = System.nanoTime();
		Map<Long, Long> balances = new TreeMap<>();
		this.db.sql("SELECT account_id, balance FROM balances WHERE account_id IN (:ids) ORDER BY account_id FOR UPDATE")
			.param("ids", new ArrayList<>(accountIds))
			.query((rs, row) -> balances.put(rs.getLong("account_id"), rs.getLong("balance")))
			.list();
		this.metrics.recordLockWait("ledger_post", System.nanoTime() - started);
		if (balances.size() != accountIds.size()) {
			throw new IllegalStateException("missing balance rows for " + accountIds);
		}
		return balances;
	}

}
