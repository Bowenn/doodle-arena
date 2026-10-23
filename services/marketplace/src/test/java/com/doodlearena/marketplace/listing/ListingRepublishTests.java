package com.doodlearena.marketplace.listing;

import java.util.List;
import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.MediaType;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ownership.MintRequest;
import com.doodlearena.marketplace.ownership.MintService;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.marketplace.support.TestTokens;
import com.doodlearena.proto.doodle.events.v1.ListingEvent;

import static org.assertj.core.api.Assertions.assertThat;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.delete;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/** Every active listing announced again, for search (ADR 0023). */
class ListingRepublishTests extends ApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private MintService mints;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private JdbcClient db;

	@Test
	void anAdminRepublishesEveryActiveListingAndNothingElse() throws Exception {
		AccountRef seller = this.fixtures.newUser();
		String active = list(seller, mint(seller), 40);
		String cancelled = list(seller, mint(seller), 50);
		this.mvc.perform(delete("/api/market/listings/" + cancelled).header("Authorization", bearer(seller.ownerId())))
			.andExpect(status().isOk());
		long before = outbox();
		long activeCount = this.db.sql("SELECT COUNT(*) FROM listings WHERE status = 'ACTIVE'").query(Long.class).single();

		republish(TestTokens.valid("u_" + UUID.randomUUID()), UUID.randomUUID().toString())
			.andExpect(status().isForbidden());
		String key = UUID.randomUUID().toString();
		String admin = admin();
		republish(admin, key).andExpect(status().isOk()).andExpect(jsonPath("$.listings").value(activeCount));

		assertThat(outbox() - before).isEqualTo(activeCount);
		List<String> republished = this.db
			.sql("SELECT payload FROM outbox WHERE topic = 'market.listings.v1' ORDER BY created_at DESC LIMIT ?")
			.param(activeCount)
			.query((rs, i) -> {
				try {
					return ListingEvent.parseFrom(rs.getBytes(1)).getListingCreated().getListingId();
				}
				catch (com.google.protobuf.InvalidProtocolBufferException ex) {
					throw new IllegalStateException(ex);
				}
			})
			.list();
		assertThat(republished).contains(active).doesNotContain(cancelled);
		// The same admin's repeated key answers the first result and writes nothing more.
		republish(admin, key).andExpect(status().isOk());
		assertThat(outbox() - before).isEqualTo(activeCount);
		assertThat(this.db.sql("SELECT status FROM listings WHERE listing_id = ?::uuid").param(active)
			.query(String.class)
			.single()).isEqualTo("ACTIVE");
	}

	private long outbox() {
		return this.db.sql("SELECT COUNT(*) FROM outbox WHERE topic = 'market.listings.v1'").query(Long.class).single();
	}

	private ResultActions republish(String token, String key) throws Exception {
		return this.mvc.perform(post("/api/market/listings/republish").header("Authorization", "Bearer " + token)
			.header("Idempotency-Key", key));
	}

	private String list(AccountRef seller, UUID drawing, long price) throws Exception {
		String body = this.mvc
			.perform(post("/api/market/listings").header("Authorization", bearer(seller.ownerId()))
				.header("Idempotency-Key", UUID.randomUUID().toString())
				.contentType(MediaType.APPLICATION_JSON)
				.content("{\"drawing_id\":\"" + drawing + "\",\"price\":" + price + "}"))
			.andExpect(status().isCreated())
			.andReturn()
			.getResponse()
			.getContentAsString();
		return com.jayway.jsonpath.JsonPath.read(body, "$.listing_id");
	}

	private UUID mint(AccountRef owner) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, owner.ownerId())));
		return drawing;
	}

	private static String admin() {
		return TestTokens.with("u_admin_" + UUID.randomUUID(), (claims) -> claims.claim("roles", List.of("player", "admin")));
	}

	private static String bearer(String user) {
		return "Bearer " + TestTokens.valid(user);
	}

}
