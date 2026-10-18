package com.doodlearena.marketplace.listing;

import java.util.UUID;

import com.jayway.jsonpath.JsonPath;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.MediaType;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.ownership.MintRequest;
import com.doodlearena.marketplace.ownership.MintService;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.marketplace.support.TestTokens;

import static org.assertj.core.api.Assertions.assertThat;
import static org.hamcrest.Matchers.nullValue;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.delete;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.header;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

class ListingApiTests extends ApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private MintService mints;

	@Autowired
	private JdbcClient db;

	@Test
	void ownerListsADrawing() throws Exception {
		String owner = user();
		UUID drawing = mint(owner);

		String body = create(owner, drawing, 50, key()).andExpect(status().isCreated())
			.andExpect(jsonPath("$.drawing_id").value(drawing.toString()))
			.andExpect(jsonPath("$.seller_id").value(owner))
			.andExpect(jsonPath("$.price").value(50))
			.andExpect(jsonPath("$.status").value("ACTIVE"))
			.andExpect(jsonPath("$.buyer_id").value(nullValue()))
			.andReturn()
			.getResponse()
			.getContentAsString();
		String listingId = JsonPath.read(body, "$.listing_id");

		this.mvc.perform(get("/api/market/listings/" + listingId).header("Authorization", bearer(user())))
			.andExpect(status().isOk())
			.andExpect(jsonPath("$.status").value("ACTIVE"));
		assertThat(outboxEvents(drawing)).isEqualTo(1);
	}

	@Test
	void sameKeyReplaysTheListing() throws Exception {
		String owner = user();
		UUID drawing = mint(owner);
		String key = key();

		String first = create(owner, drawing, 50, key).andExpect(status().isCreated()).andReturn().getResponse().getContentAsString();
		String second = create(owner, drawing, 50, key).andExpect(status().isCreated())
			.andExpect(header().string("Idempotency-Replayed", "true"))
			.andReturn()
			.getResponse()
			.getContentAsString();

		assertThat(second).isEqualTo(first);
		assertThat(outboxEvents(drawing)).isEqualTo(1);
		create(owner, drawing, 60, key).andExpect(status().isUnprocessableContent())
			.andExpect(jsonPath("$.error.code").value("IDEMPOTENCY_KEY_REUSED"));
	}

	@Test
	void onlyTheOwnerListsAndOnlyOnce() throws Exception {
		String owner = user();
		UUID drawing = mint(owner);

		create(user(), drawing, 50, key()).andExpect(status().isForbidden())
			.andExpect(jsonPath("$.error.code").value("NOT_OWNER"));
		create(owner, UUID.randomUUID(), 50, key()).andExpect(status().isNotFound())
			.andExpect(jsonPath("$.error.code").value("DRAWING_NOT_FOUND"));
		create(owner, drawing, 50, key()).andExpect(status().isCreated());
		create(owner, drawing, 70, key()).andExpect(status().isConflict())
			.andExpect(jsonPath("$.error.code").value("ALREADY_LISTED"));
	}

	@Test
	void missingKeyIsRefused() throws Exception {
		String owner = user();
		UUID drawing = mint(owner);
		this.mvc.perform(post("/api/market/listings").header("Authorization", bearer(owner))
			.contentType(MediaType.APPLICATION_JSON)
			.content(body(drawing, 50))).andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("IDEMPOTENCY_KEY_REQUIRED"));
	}

	@ParameterizedTest
	@ValueSource(strings = { "", "{}", "not json", "{\"drawing_id\":\"x\",\"price\":5}", "{\"drawing_id\":\"%s\",\"price\":0}",
			"{\"drawing_id\":\"%s\",\"price\":1000001}", "{\"drawing_id\":\"%s\",\"price\":1.5}",
			"{\"drawing_id\":\"%s\",\"price\":\"5\"}", "{\"drawing_id\":\"%s\"}" })
	void badBodyIsInvalidRequest(String template) throws Exception {
		String owner = user();
		String content = template.formatted(mint(owner));
		this.mvc.perform(post("/api/market/listings").header("Authorization", bearer(owner))
			.header("Idempotency-Key", key())
			.contentType(MediaType.APPLICATION_JSON)
			.content(content)).andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("INVALID_REQUEST"));
	}

	@Test
	void sellerCancelsAndCancellingAgainIsTheSame() throws Exception {
		String owner = user();
		UUID drawing = mint(owner);
		String listingId = listingId(create(owner, drawing, 50, key()));

		this.mvc.perform(delete("/api/market/listings/" + listingId).header("Authorization", bearer(user())))
			.andExpect(status().isForbidden())
			.andExpect(jsonPath("$.error.code").value("NOT_OWNER"));
		this.mvc.perform(delete("/api/market/listings/" + listingId).header("Authorization", bearer(owner)))
			.andExpect(status().isOk())
			.andExpect(jsonPath("$.status").value("CANCELLED"))
			.andExpect(jsonPath("$.closed_at").isNotEmpty());
		this.mvc.perform(delete("/api/market/listings/" + listingId).header("Authorization", bearer(owner)))
			.andExpect(status().isOk())
			.andExpect(jsonPath("$.status").value("CANCELLED"));
		this.mvc.perform(delete("/api/market/listings/" + UUID.randomUUID()).header("Authorization", bearer(owner)))
			.andExpect(status().isNotFound())
			.andExpect(jsonPath("$.error.code").value("LISTING_NOT_FOUND"));
		// Created and cancelled: two events, and the drawing can be listed again.
		assertThat(outboxEvents(drawing)).isEqualTo(2);
		create(owner, drawing, 80, key()).andExpect(status().isCreated());
	}

	@Test
	void activeListingsPageNewestFirst() throws Exception {
		String owner = user();
		UUID first = mint(owner);
		UUID second = mint(owner);
		UUID third = mint(owner);
		create(owner, first, 10, key());
		create(owner, second, 20, key());
		String cancelled = listingId(create(owner, third, 30, key()));
		this.mvc.perform(delete("/api/market/listings/" + cancelled).header("Authorization", bearer(owner)));

		String page = this.mvc.perform(get("/api/market/listings?limit=1&seller_id=" + owner).header("Authorization", bearer(owner)))
			.andExpect(status().isOk())
			.andExpect(jsonPath("$.listings.length()").value(1))
			.andExpect(jsonPath("$.listings[0].drawing_id").value(second.toString()))
			.andReturn()
			.getResponse()
			.getContentAsString();
		String cursor = JsonPath.read(page, "$.next_cursor");
		this.mvc.perform(get("/api/market/listings?limit=1&seller_id=" + owner + "&cursor=" + cursor)
			.header("Authorization", bearer(owner)))
			.andExpect(jsonPath("$.listings[0].drawing_id").value(first.toString()))
			.andExpect(jsonPath("$.next_cursor").value(nullValue()));
		this.mvc.perform(get("/api/market/listings?drawing_id=" + third).header("Authorization", bearer(owner)))
			.andExpect(jsonPath("$.listings").isEmpty());
		this.mvc.perform(get("/api/market/listings?limit=0").header("Authorization", bearer(owner)))
			.andExpect(status().isBadRequest());
	}

	private ResultActions create(String seller, UUID drawing, long price, String key) throws Exception {
		return this.mvc.perform(post("/api/market/listings").header("Authorization", bearer(seller))
			.header("Idempotency-Key", key)
			.contentType(MediaType.APPLICATION_JSON)
			.content(body(drawing, price)));
	}

	private static String body(UUID drawing, long price) {
		return "{\"drawing_id\":\"" + drawing + "\",\"price\":" + price + "}";
	}

	private static String listingId(ResultActions result) throws Exception {
		return JsonPath.read(result.andReturn().getResponse().getContentAsString(), "$.listing_id");
	}

	private UUID mint(String owner) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, owner)));
		return drawing;
	}

	private long outboxEvents(UUID drawing) {
		return this.db.sql("SELECT COUNT(*) FROM outbox WHERE topic = 'market.listings.v1' AND key = ?")
			.param(drawing.toString())
			.query(Long.class)
			.single();
	}

	private static String user() {
		return "u_" + UUID.randomUUID();
	}

	private static String key() {
		return UUID.randomUUID().toString();
	}

	private static String bearer(String user) {
		return "Bearer " + TestTokens.valid(user);
	}

}
