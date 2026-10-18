package com.doodlearena.marketplace.wallet;

import java.time.Instant;
import java.time.OffsetDateTime;
import java.util.List;
import java.util.UUID;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Repository;

/** Reads a user's wallet. Reads never create an account: a user without one has 0 coins. */
@Repository
public class WalletRepository {

	private final JdbcClient db;

	public WalletRepository(JdbcClient db) {
		this.db = db;
	}

	public Balance balance(String userId) {
		return this.db.sql("""
				SELECT b.balance, b.updated_at
				FROM accounts a JOIN balances b USING (account_id, owner_type)
				WHERE a.owner_type = 'user' AND a.owner_id = ?""")
			.param(userId)
			.query((rs, row) -> new Balance(rs.getLong("balance"), rs.getObject("updated_at", OffsetDateTime.class).toInstant()))
			.optional()
			.orElse(new Balance(0, null));
	}

	/** The user's entries, newest first, starting below {@code beforeEntryId} when it is set. */
	public List<Entry> entries(String userId, Long beforeEntryId, int limit) {
		return this.db.sql("""
				SELECT e.entry_id, e.tx_id, t.type, e.amount, t.created_at
				FROM accounts a
				JOIN ledger_entries e ON e.account_id = a.account_id
				JOIN ledger_transactions t ON t.tx_id = e.tx_id
				WHERE a.owner_type = 'user' AND a.owner_id = :user
					AND (CAST(:before AS BIGINT) IS NULL OR e.entry_id < :before)
				ORDER BY e.entry_id DESC
				LIMIT :limit""")
			.param("user", userId)
			.param("before", beforeEntryId)
			.param("limit", limit)
			.query((rs, row) -> new Entry(rs.getLong("entry_id"), rs.getObject("tx_id", UUID.class),
					rs.getString("type"), rs.getLong("amount"),
					rs.getObject("created_at", OffsetDateTime.class).toInstant()))
			.list();
	}

	public record Balance(long balance, Instant updatedAt) {
	}

	public record Entry(long entryId, UUID txId, String type, long amount, Instant createdAt) {
	}

}
