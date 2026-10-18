package com.doodlearena.marketplace.wallet;

import java.time.Duration;
import java.time.Instant;
import java.util.Date;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import com.jayway.jsonpath.JsonPath;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.TestTokens;

import static com.doodlearena.marketplace.ledger.Posting.credit;
import static com.doodlearena.marketplace.ledger.Posting.debit;
import static org.assertj.core.api.Assertions.assertThat;
import static org.hamcrest.Matchers.nullValue;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/** Token verification (ADR 0001) and the wallet endpoints, through the real filter chain. */
class WalletApiTests extends ApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private LedgerService ledger;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private JdbcClient db;

	@Test
	void healthzNeedsNoToken() throws Exception {
		this.mvc.perform(get("/healthz")).andExpect(status().isOk());
	}

	@Test
	void missingTokenIsUnauthenticated() throws Exception {
		unauthenticated(this.mvc.perform(get("/api/market/wallet").header("X-Request-Id", "req-1")))
			.andExpect(jsonPath("$.error.request_id").value("req-1"));
	}

	@Test
	void newUserHasAnEmptyWalletAndNoAccount() throws Exception {
		String user = "u_" + UUID.randomUUID();
		wallet(user).andExpect(status().isOk())
			.andExpect(jsonPath("$.user_id").value(user))
			.andExpect(jsonPath("$.balance").value(0))
			.andExpect(jsonPath("$.updated_at").value(nullValue()));
		transactions(user, "").andExpect(status().isOk())
			.andExpect(jsonPath("$.transactions").isEmpty())
			.andExpect(jsonPath("$.next_cursor").value(nullValue()));
		assertThat(this.db.sql("SELECT COUNT(*) FROM accounts WHERE owner_id = ?").param(user).query(Long.class).single())
			.isZero();
	}

	@Test
	void walletShowsBalanceAndHistoryNewestFirst() throws Exception {
		AccountRef user = this.fixtures.newUser();
		AccountRef other = this.fixtures.newUser();
		reward(user, 30);
		reward(user, 20);
		this.ledger.post(TransactionType.SALE, "sale:" + UUID.randomUUID(), Map.of(),
				List.of(debit(user, 5), credit(other, 5)));

		wallet(user.ownerId()).andExpect(jsonPath("$.balance").value(45))
			.andExpect(jsonPath("$.updated_at").isNotEmpty());

		String body = transactions(user.ownerId(), "?limit=2").andExpect(status().isOk())
			.andExpect(jsonPath("$.transactions.length()").value(2))
			.andExpect(jsonPath("$.transactions[0].type").value("sale"))
			.andExpect(jsonPath("$.transactions[0].amount").value(-5))
			.andExpect(jsonPath("$.transactions[0].tx_id").isNotEmpty())
			.andExpect(jsonPath("$.transactions[0].created_at").isNotEmpty())
			.andExpect(jsonPath("$.transactions[1].type").value("reward"))
			.andExpect(jsonPath("$.transactions[1].amount").value(20))
			.andReturn()
			.getResponse()
			.getContentAsString();
		String cursor = JsonPath.read(body, "$.next_cursor");

		transactions(user.ownerId(), "?limit=2&cursor=" + cursor).andExpect(status().isOk())
			.andExpect(jsonPath("$.transactions.length()").value(1))
			.andExpect(jsonPath("$.transactions[0].amount").value(30))
			.andExpect(jsonPath("$.next_cursor").value(nullValue()));
		// The other user's wallet shows only their side.
		transactions(other.ownerId(), "").andExpect(jsonPath("$.transactions.length()").value(1))
			.andExpect(jsonPath("$.transactions[0].amount").value(5));
	}

	@ParameterizedTest
	@ValueSource(strings = { "?limit=0", "?limit=51", "?limit=abc", "?cursor=***", "?cursor=YWJj", "?cursor=LTE" })
	void badQueryIsInvalidRequest(String query) throws Exception {
		transactions("u_" + UUID.randomUUID(), query).andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("INVALID_REQUEST"));
	}

	@Test
	void expiredTokenIsRefused() throws Exception {
		// Expired 5 minutes ago: past the skew.
		Instant past = Instant.now().minus(Duration.ofMinutes(20));
		unauthenticated(withToken(TestTokens.with("u_x",
				(claims) -> claims.issueTime(Date.from(past)).expirationTime(Date.from(past.plus(Duration.ofMinutes(15)))))));
	}

	@Test
	void tokenWithinClockSkewIsAccepted() throws Exception {
		// Expired 10 seconds ago: within ADR 0001's 30 seconds of skew.
		Instant justExpired = Instant.now().minus(Duration.ofSeconds(10));
		withToken(TestTokens.with("u_x", (claims) -> claims.issueTime(Date.from(justExpired.minus(Duration.ofMinutes(15))))
			.expirationTime(Date.from(justExpired))))
			.andExpect(status().isOk());
	}

	@Test
	void wrongAudienceIsRefused() throws Exception {
		unauthenticated(withToken(TestTokens.with("u_x", (claims) -> claims.audience("another-api"))));
	}

	@Test
	void wrongIssuerIsRefused() throws Exception {
		unauthenticated(withToken(TestTokens.with("u_x", (claims) -> claims.issuer("http://evil.example/api/auth"))));
	}

	@Test
	void tokenWithoutExpiryIsRefused() throws Exception {
		unauthenticated(withToken(TestTokens.with("u_x", (claims) -> claims.expirationTime(null))));
	}

	@Test
	void tokenWithoutSubjectIsRefused() throws Exception {
		unauthenticated(withToken(TestTokens.with("u_x", (claims) -> claims.subject(null))));
	}

	@Test
	void keyOutsideTheJwksIsRefused() throws Exception {
		unauthenticated(withToken(TestTokens.foreignKey("u_x")));
	}

	@Test
	void hmacTokenIsRefused() throws Exception {
		unauthenticated(withToken(TestTokens.hs256("u_x")));
	}

	@Test
	void unsignedTokenIsRefused() throws Exception {
		unauthenticated(withToken(TestTokens.unsigned("u_x")));
	}

	@Test
	void unknownPathAndMethodUseTheErrorFormat() throws Exception {
		this.mvc.perform(get("/api/market/nothing-here").header("Authorization", "Bearer " + TestTokens.valid("u_x")))
			.andExpect(status().isNotFound())
			.andExpect(jsonPath("$.error.code").value("NOT_FOUND"));
		this.mvc.perform(post("/api/market/wallet").header("Authorization", "Bearer " + TestTokens.valid("u_x")))
			.andExpect(status().isMethodNotAllowed())
			.andExpect(jsonPath("$.error.code").value("METHOD_NOT_ALLOWED"));
	}

	private void reward(AccountRef user, long coins) {
		this.ledger.post(TransactionType.REWARD, "reward:" + UUID.randomUUID(), Map.of(),
				List.of(debit(AccountRef.REWARDS_ISSUANCE, coins), credit(user, coins)));
	}

	private ResultActions wallet(String userId) throws Exception {
		return withToken(TestTokens.valid(userId));
	}

	private ResultActions withToken(String token) throws Exception {
		return this.mvc.perform(get("/api/market/wallet").header("Authorization", "Bearer " + token));
	}

	private ResultActions transactions(String userId, String query) throws Exception {
		return this.mvc.perform(get("/api/market/wallet/transactions" + query).header("Authorization",
				"Bearer " + TestTokens.valid(userId)));
	}

	private static ResultActions unauthenticated(ResultActions result) throws Exception {
		return result.andExpect(status().isUnauthorized()).andExpect(jsonPath("$.error.code").value("UNAUTHENTICATED"));
	}

}
