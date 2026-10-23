package com.doodlearena.marketplace.tip;

import java.util.List;
import java.util.UUID;

import com.jayway.jsonpath.JsonPath;
import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.MediaType;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.TestTokens;

import static org.assertj.core.api.Assertions.assertThat;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.header;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/** {@code POST /api/market/tips} through the whole HTTP stack (ADR 0021). */
class TipApiTests extends ApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private ReconciliationJob reconciliation;

	@Autowired
	private TipSettlement settlement;

	@Autowired
	private JdbcClient db;

	@Test
	void aTipMovesTheTippersCoinsIntoOneEscrowShard() throws Exception {
		AccountRef viewer = this.fixtures.newUserWith(100);
		String match = "m_" + UUID.randomUUID();

		String body = tip(viewer, "r_1", match, 1, 30, key()).andExpect(status().isCreated())
			.andExpect(jsonPath("$.room_id").value("r_1"))
			.andExpect(jsonPath("$.match_id").value(match))
			.andExpect(jsonPath("$.round_no").value(1))
			.andExpect(jsonPath("$.amount").value(30))
			.andExpect(jsonPath("$.status").value("PENDING"))
			.andReturn()
			.getResponse()
			.getContentAsString();

		UUID tipId = UUID.fromString(JsonPath.read(body, "$.tip_id"));
		assertThat(this.fixtures.balance(viewer)).isEqualTo(70);
		int shard = this.db.sql("SELECT shard FROM tips WHERE tip_id = ?").param(tipId).query(Integer.class).single();
		assertThat(shard).isEqualTo(TipService.shardOf(tipId));
		assertThat(this.fixtures.transactionsWithKey("tip:" + tipId)).isOne();
		assertThat(this.reconciliation.reconcile().clean()).isTrue();
	}

	@Test
	void aTipBeyondTheWalletMovesNothing() throws Exception {
		AccountRef viewer = this.fixtures.newUserWith(10);
		String match = "m_" + UUID.randomUUID();

		tip(viewer, "r_1", match, 1, 11, key()).andExpect(status().isUnprocessableContent())
			.andExpect(jsonPath("$.error.code").value("INSUFFICIENT_FUNDS"));

		assertThat(this.fixtures.balance(viewer)).isEqualTo(10);
		assertThat(pendingTips(match)).isZero();
	}

	@Test
	void aRepeatedKeyReturnsTheFirstTipAndMovesCoinsOnce() throws Exception {
		AccountRef viewer = this.fixtures.newUserWith(100);
		String match = "m_" + UUID.randomUUID();
		String key = key();

		String first = tip(viewer, "r_1", match, 1, 25, key).andExpect(status().isCreated())
			.andReturn()
			.getResponse()
			.getContentAsString();
		tip(viewer, "r_1", match, 1, 25, key).andExpect(status().isCreated())
			.andExpect(header().string("Idempotency-Replayed", "true"))
			.andExpect(jsonPath("$.tip_id").value((String) JsonPath.read(first, "$.tip_id")));
		// The same key for another tip is a client bug, not a second tip.
		tip(viewer, "r_1", match, 1, 26, key).andExpect(status().isUnprocessableContent())
			.andExpect(jsonPath("$.error.code").value("IDEMPOTENCY_KEY_REUSED"));

		assertThat(this.fixtures.balance(viewer)).isEqualTo(75);
		assertThat(pendingTips(match)).isOne();
	}

	@Test
	void aTipNeedsAKeyAndAWellFormedBody() throws Exception {
		AccountRef viewer = this.fixtures.newUserWith(5000);
		String match = "m_" + UUID.randomUUID();

		this.mvc
			.perform(post("/api/market/tips").header("Authorization", bearer(viewer))
				.contentType(MediaType.APPLICATION_JSON)
				.content(body("r_1", match, 1, "5")))
			.andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("IDEMPOTENCY_KEY_REQUIRED"));
		for (String amount : List.of("0", "1001", "1.5", "\"5\"", "-3")) {
			send(viewer, body("r_1", match, 1, amount)).andExpect(status().isBadRequest())
				.andExpect(jsonPath("$.error.code").value("INVALID_REQUEST"));
		}
		send(viewer, body("r_1", match, 0, "5")).andExpect(status().isBadRequest());
		send(viewer, body("", match, 1, "5")).andExpect(status().isBadRequest());
		send(viewer, "{\"room_id\":\"r_1\",\"round_no\":1,\"amount\":5}").andExpect(status().isBadRequest());
		send(viewer, "not json").andExpect(status().isBadRequest());
		send(viewer, body("r_1", match, 1, "1000")).andExpect(status().isCreated());

		assertThat(this.fixtures.balance(viewer)).isEqualTo(4000);
	}

	@Test
	void aRoundSettledMoreThanThirtySecondsAgoTakesNoTips() throws Exception {
		AccountRef viewer = this.fixtures.newUserWith(100);
		String match = "m_" + UUID.randomUUID();
		this.settlement.roundFinished(new RoundRequest(match, "r_1", 1, List.of(this.fixtures.newUser().ownerId())));

		// Just settled: a tip sent as the round ended still counts (the sweeper pays it).
		tip(viewer, "r_1", match, 1, 5, key()).andExpect(status().isCreated());
		this.db.sql("UPDATE tip_rounds SET created_at = now() - interval '31 seconds' WHERE match_id = ?")
			.param(match)
			.update();
		tip(viewer, "r_1", match, 1, 5, key()).andExpect(status().isConflict())
			.andExpect(jsonPath("$.error.code").value("ROUND_OVER"));

		assertThat(this.fixtures.balance(viewer)).isEqualTo(95);
	}

	@Test
	void aTipNeedsAToken() throws Exception {
		this.mvc
			.perform(post("/api/market/tips").header("Idempotency-Key", key())
				.contentType(MediaType.APPLICATION_JSON)
				.content(body("r_1", "m_1", 1, "5")))
			.andExpect(status().isUnauthorized());
	}

	private long pendingTips(String match) {
		return this.db.sql("SELECT COUNT(*) FROM tips WHERE match_id = ? AND status = 'PENDING'")
			.param(match)
			.query(Long.class)
			.single();
	}

	private ResultActions tip(AccountRef viewer, String room, String match, int round, long amount, String key)
			throws Exception {
		return this.mvc.perform(post("/api/market/tips").header("Authorization", bearer(viewer))
			.header("Idempotency-Key", key)
			.contentType(MediaType.APPLICATION_JSON)
			.content(body(room, match, round, Long.toString(amount))));
	}

	private ResultActions send(AccountRef viewer, String body) throws Exception {
		return this.mvc.perform(post("/api/market/tips").header("Authorization", bearer(viewer))
			.header("Idempotency-Key", key())
			.contentType(MediaType.APPLICATION_JSON)
			.content(body));
	}

	private static String body(String room, String match, int round, String amount) {
		return "{\"room_id\":\"" + room + "\",\"match_id\":\"" + match + "\",\"round_no\":" + round + ",\"amount\":"
				+ amount + "}";
	}

	private static String key() {
		return UUID.randomUUID().toString();
	}

	private static String bearer(AccountRef user) {
		return "Bearer " + TestTokens.valid(user.ownerId());
	}

}
