package com.doodlearena.marketplace.drop;

import java.time.Duration;
import java.time.Instant;
import java.util.List;
import java.util.UUID;

import com.jayway.jsonpath.JsonPath;
import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.http.MediaType;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.ownership.MintRequest;
import com.doodlearena.marketplace.ownership.MintService;
import com.doodlearena.marketplace.support.DropTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.marketplace.support.TestTokens;

import static org.assertj.core.api.Assertions.assertThat;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.header;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/** Limited drops through the whole HTTP stack (ADR 0022). */
class DropApiTests extends DropTestSupport {

	@Autowired
	private MockMvc mvc;

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

	@Test
	void onlyAnAdminCreatesADropOfAnApprovedDrawing() throws Exception {
		UUID drawing = mint(this.fixtures.newUser());
		String opens = Instant.now().plus(Duration.ofMinutes(5)).toString();
		String body = "{\"drawing_id\":\"" + drawing + "\",\"editions\":10,\"price\":25,\"opens_at\":\"" + opens
				+ "\",\"duration_minutes\":60}";

		create(player(), body).andExpect(status().isForbidden()).andExpect(jsonPath("$.error.code").value("FORBIDDEN"));
		create(admin(), body).andExpect(status().isCreated())
			.andExpect(jsonPath("$.drawing_id").value(drawing.toString()))
			.andExpect(jsonPath("$.editions").value(10))
			.andExpect(jsonPath("$.editions_left").value(10))
			.andExpect(jsonPath("$.price").value(25))
			.andExpect(jsonPath("$.status").value("UPCOMING"));

		create(admin(), body.replace(drawing.toString(), UUID.randomUUID().toString()))
			.andExpect(status().isNotFound())
			.andExpect(jsonPath("$.error.code").value("DRAWING_NOT_FOUND"));
		create(admin(), body.replace(opens, Instant.now().plusSeconds(5).toString())).andExpect(status().isBadRequest());
		create(admin(), body.replace("\"editions\":10", "\"editions\":1001")).andExpect(status().isBadRequest());
		create(admin(), body.replace("\"price\":25", "\"price\":2.5")).andExpect(status().isBadRequest());
	}

	@Test
	void anAdmittedBuyerGetsANumberedEditionAndTheCoinsWaitInEscrow() throws Exception {
		Drop drop = openDrop(3, 40);
		AccountRef buyer = this.fixtures.newUserWith(100);

		join(buyer, drop).andExpect(status().isOk())
			.andExpect(jsonPath("$.admitted").value(true))
			.andExpect(jsonPath("$.position").value(0));
		String body = buy(buyer, drop, key()).andExpect(status().isCreated())
			.andExpect(jsonPath("$.drop_id").value(drop.dropId().toString()))
			.andExpect(jsonPath("$.editions").value(3))
			.andExpect(jsonPath("$.price").value(40))
			.andReturn()
			.getResponse()
			.getContentAsString();

		int edition = JsonPath.read(body, "$.edition_no");
		assertThat(edition).isBetween(1, 3);
		assertThat(this.fixtures.balance(buyer)).isEqualTo(60);
		assertThat(this.fixtures.balance(AccountRef.dropEscrow((edition - 1) % 16))).isGreaterThanOrEqualTo(40);
		this.mvc.perform(get("/api/market/editions?owner=me").header("Authorization", bearer(buyer)))
			.andExpect(status().isOk())
			.andExpect(jsonPath("$.editions[0].edition_no").value(edition))
			.andExpect(jsonPath("$.editions[0].drawing_id").value(drop.drawingId().toString()));
		this.mvc.perform(get("/api/market/drops/" + drop.dropId()).header("Authorization", bearer(buyer)))
			.andExpect(jsonPath("$.editions_left").value(2))
			.andExpect(jsonPath("$.status").value("OPEN"));
		assertThat(this.reconciliation.reconcile().clean()).isTrue();
	}

	@Test
	void oneEditionPerBuyerAndARepeatedKeyReturnsTheSameEdition() throws Exception {
		Drop drop = openDrop(5, 10);
		AccountRef buyer = this.fixtures.newUserWith(100);
		join(buyer, drop);
		String key = key();

		String first = buy(buyer, drop, key).andExpect(status().isCreated()).andReturn().getResponse().getContentAsString();
		buy(buyer, drop, key).andExpect(status().isCreated())
			.andExpect(header().string("Idempotency-Replayed", "true"))
			.andExpect(jsonPath("$.edition_no").value((Integer) JsonPath.read(first, "$.edition_no")));
		buy(buyer, drop, key()).andExpect(status().isConflict())
			.andExpect(jsonPath("$.error.code").value("ALREADY_BOUGHT"));

		assertThat(this.fixtures.balance(buyer)).isEqualTo(90);
		assertThat(sold(drop)).isOne();
	}

	@Test
	void aBuyerRedisForgotGetsNoSecondEditionAndLosesNone() throws Exception {
		Drop drop = openDrop(4, 10);
		AccountRef buyer = this.fixtures.newUserWith(100);
		join(buyer, drop);
		String first = buy(buyer, drop, key()).andReturn().getResponse().getContentAsString();
		int owned = JsonPath.read(first, "$.edition_no");
		// Redis forgets who bought (a flush): the buyer's next try reserves a new edition.
		this.redis.delete(DropGate.key(drop.dropId(), "buyers"));

		buy(buyer, drop, key()).andExpect(status().isConflict())
			.andExpect(jsonPath("$.error.code").value("ALREADY_BOUGHT"));

		// That edition went back for someone else, and Redis learned the one they own.
		assertThat(this.redis.opsForList().size(DropGate.key(drop.dropId(), "free"))).isEqualTo(3);
		assertThat(this.redis.opsForHash().get(DropGate.key(drop.dropId(), "buyers"), buyer.ownerId()))
			.isEqualTo("p:" + owned);
		assertThat(this.fixtures.balance(buyer)).isEqualTo(90);
	}

	@Test
	void aBuyerMustJoinAndBeAdmitted() throws Exception {
		Drop drop = openDrop(5, 10);
		AccountRef buyer = this.fixtures.newUserWith(100);

		buy(buyer, drop, key()).andExpect(status().isForbidden()).andExpect(jsonPath("$.error.code").value("NOT_ADMITTED"));

		// A drop that admits one place a second, with someone already ahead.
		Drop slow = this.drops.insert("u_admin", mint(this.fixtures.newUser()), 5, 10, Instant.now().minusMillis(200),
				Instant.now().plus(Duration.ofHours(1)), 1, Instant.now());
		join(this.fixtures.newUserWith(100), slow);
		join(buyer, slow).andExpect(jsonPath("$.admitted").value(false)).andExpect(jsonPath("$.position").value(1));
		buy(buyer, slow, key()).andExpect(status().isForbidden())
			.andExpect(jsonPath("$.error.code").value("NOT_ADMITTED"));
		assertThat(this.fixtures.balance(buyer)).isEqualTo(100);
	}

	@Test
	void aBuyerWithoutTheCoinsGivesTheEditionBack() throws Exception {
		Drop drop = openDrop(1, 50);
		AccountRef poor = this.fixtures.newUserWith(10);
		join(poor, drop);

		buy(poor, drop, key()).andExpect(status().isUnprocessableContent())
			.andExpect(jsonPath("$.error.code").value("INSUFFICIENT_FUNDS"));

		assertThat(this.redis.opsForList().size(DropGate.key(drop.dropId(), "free"))).isOne();
		AccountRef rich = this.fixtures.newUserWith(100);
		join(rich, drop);
		buy(rich, drop, key()).andExpect(status().isCreated()).andExpect(jsonPath("$.edition_no").value(1));
		// Sold out is told before anything else, even to someone who never joined.
		buy(this.fixtures.newUserWith(100), drop, key()).andExpect(status().isConflict())
			.andExpect(jsonPath("$.error.code").value("SOLD_OUT"));
		AccountRef late = this.fixtures.newUserWith(100);
		join(late, drop);
		buy(late, drop, key()).andExpect(status().isConflict()).andExpect(jsonPath("$.error.code").value("SOLD_OUT"));
		assertThat(this.fixtures.balance(late)).isEqualTo(100);
	}

	@Test
	void aDropSellsOnlyWhileItIsOpen() throws Exception {
		AccountRef buyer = this.fixtures.newUserWith(100);
		Drop upcoming = this.drops.insert("u_admin", mint(this.fixtures.newUser()), 5, 10,
				Instant.now().plus(Duration.ofMinutes(5)), Instant.now().plus(Duration.ofHours(1)), 50, Instant.now());
		join(buyer, upcoming).andExpect(jsonPath("$.admitted").value(false));
		buy(buyer, upcoming, key()).andExpect(status().isConflict())
			.andExpect(jsonPath("$.error.code").value("DROP_NOT_OPEN"));

		Drop closed = this.drops.insert("u_admin", mint(this.fixtures.newUser()), 5, 10,
				Instant.now().minus(Duration.ofHours(2)), Instant.now().minus(Duration.ofHours(1)), 50, Instant.now());
		join(buyer, closed).andExpect(status().isConflict()).andExpect(jsonPath("$.error.code").value("DROP_CLOSED"));
		buy(buyer, closed, key()).andExpect(status().isConflict()).andExpect(jsonPath("$.error.code").value("DROP_CLOSED"));
		assertThat(this.fixtures.balance(buyer)).isEqualTo(100);
	}

	@Test
	void eachUserHasATokenBucket() throws Exception {
		Drop drop = openDrop(5, 10);
		AccountRef user = this.fixtures.newUser();
		int limited = 0;
		String retryAfter = null;
		for (int i = 0; i < 20; i++) {
			var response = this.mvc
				.perform(get("/api/market/drops/" + drop.dropId() + "/queue").header("Authorization", bearer(user)))
				.andReturn()
				.getResponse();
			if (response.getStatus() == 429) {
				limited++;
				retryAfter = response.getHeader("Retry-After");
			}
		}
		// A burst of 10, then 5 a second: some of 20 quick requests are refused, with a Retry-After.
		assertThat(limited).isGreaterThan(0);
		assertThat(retryAfter).isNotNull();
		// Someone else's bucket is full.
		this.mvc.perform(get("/api/market/drops/" + drop.dropId() + "/queue").header("Authorization",
				bearer(this.fixtures.newUser()))).andExpect(status().isOk());
	}

	private Drop openDrop(int editions, long price) {
		return this.drops.insert("u_admin", mint(this.fixtures.newUser()), editions, price,
				Instant.now().minusMillis(500), Instant.now().plus(Duration.ofHours(1)), 50, Instant.now());
	}

	private UUID mint(AccountRef owner) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, owner.ownerId())));
		return drawing;
	}

	private long sold(Drop drop) {
		return this.db.sql("SELECT COUNT(*) FROM drop_editions WHERE drop_id = ?").param(drop.dropId())
			.query(Long.class)
			.single();
	}

	private ResultActions create(String token, String body) throws Exception {
		return this.mvc.perform(post("/api/market/drops").header("Authorization", "Bearer " + token)
			.header("Idempotency-Key", key())
			.contentType(MediaType.APPLICATION_JSON)
			.content(body));
	}

	private ResultActions join(AccountRef user, Drop drop) throws Exception {
		return this.mvc.perform(post("/api/market/drops/" + drop.dropId() + "/join").header("Authorization", bearer(user)));
	}

	private ResultActions buy(AccountRef user, Drop drop, String key) throws Exception {
		return this.mvc.perform(post("/api/market/drops/" + drop.dropId() + "/purchase").header("Authorization", bearer(user))
			.header("Idempotency-Key", key));
	}

	private static String player() {
		return TestTokens.valid("u_" + UUID.randomUUID());
	}

	private static String admin() {
		return TestTokens.with("u_admin_" + UUID.randomUUID(), (claims) -> claims.claim("roles", List.of("player", "admin")));
	}

	private static String key() {
		return UUID.randomUUID().toString();
	}

	private static String bearer(AccountRef user) {
		return "Bearer " + TestTokens.valid(user.ownerId());
	}

}
