package com.doodlearena.marketplace.ownership;

import java.time.Instant;
import java.time.ZoneOffset;
import java.util.UUID;

import com.jayway.jsonpath.JsonPath;
import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.listing.Listing;
import com.doodlearena.marketplace.listing.ListingService;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.marketplace.support.TestTokens;

import static org.hamcrest.Matchers.nullValue;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/** "My drawings": {@code GET /api/market/drawings?owner=me}. */
class OwnedDrawingsApiTests extends ApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private MintService mints;

	@Autowired
	private ListingService listings;

	@Autowired
	private JdbcClient db;

	@Test
	void listsOnlyTheCallersDrawingsWithTheirActiveListing() throws Exception {
		String me = "u_own_" + UUID.randomUUID();
		UUID listed = mint(me, Instant.parse("2026-01-01T00:00:00Z"));
		UUID plain = mint(me, Instant.parse("2026-01-02T00:00:00Z"));
		mint("u_someone_else", Instant.parse("2026-01-03T00:00:00Z"));
		Listing listing = this.listings.create(me, listed, 250);

		owned(me, "").andExpect(status().isOk())
			.andExpect(jsonPath("$.drawings.length()").value(2))
			.andExpect(jsonPath("$.drawings[0].drawing_id").value(plain.toString()))
			.andExpect(jsonPath("$.drawings[0].version").value(1))
			.andExpect(jsonPath("$.drawings[0].minted_at").isNotEmpty())
			.andExpect(jsonPath("$.drawings[0].listing").value(nullValue()))
			.andExpect(jsonPath("$.drawings[1].drawing_id").value(listed.toString()))
			.andExpect(jsonPath("$.drawings[1].listing.listing_id").value(listing.listingId().toString()))
			.andExpect(jsonPath("$.drawings[1].listing.price").value(250))
			.andExpect(jsonPath("$.next_cursor").value(nullValue()));
	}

	@Test
	void aCancelledListingIsNotShown() throws Exception {
		String me = "u_own_" + UUID.randomUUID();
		UUID drawing = mint(me, Instant.parse("2026-01-01T00:00:00Z"));
		Listing listing = this.listings.create(me, drawing, 99);
		this.listings.cancel(me, listing.listingId());

		owned(me, "").andExpect(status().isOk())
			.andExpect(jsonPath("$.drawings[0].listing").value(nullValue()));
	}

	@Test
	void pagesNewestAcquisitionFirstWithoutGapsOrRepeats() throws Exception {
		String me = "u_own_" + UUID.randomUUID();
		Instant same = Instant.parse("2026-02-01T10:00:00.123456Z");
		// Two drawings acquired in the same microsecond: the drawing id breaks the tie.
		UUID a = mint(me, same);
		UUID b = mint(me, same);
		UUID older = mint(me, Instant.parse("2026-01-15T00:00:00Z"));
		UUID newest = mint(me, Instant.parse("2026-03-01T00:00:00Z"));
		// Postgres compares uuids bytewise and Java signed: ask Postgres which one comes first.
		String first = this.db.sql("SELECT id::text FROM unnest(ARRAY[?, ?]::uuid[]) AS t(id) ORDER BY id DESC LIMIT 1")
			.params(a, b)
			.query(String.class)
			.single();
		UUID[] tied = first.equals(a.toString()) ? new UUID[] { a, b } : new UUID[] { b, a };

		String page1 = owned(me, "&limit=2").andExpect(status().isOk())
			.andExpect(jsonPath("$.drawings[0].drawing_id").value(newest.toString()))
			.andExpect(jsonPath("$.drawings[1].drawing_id").value(tied[0].toString()))
			.andReturn()
			.getResponse()
			.getContentAsString();
		String cursor = JsonPath.read(page1, "$.next_cursor");

		// A full last page still knows it's the last: the query reads one row past the limit.
		owned(me, "&limit=2&cursor=" + cursor).andExpect(status().isOk())
			.andExpect(jsonPath("$.drawings.length()").value(2))
			.andExpect(jsonPath("$.drawings[0].drawing_id").value(tied[1].toString()))
			.andExpect(jsonPath("$.drawings[1].drawing_id").value(older.toString()))
			.andExpect(jsonPath("$.next_cursor").value(nullValue()));
	}

	@Test
	void onlyOwnerMe() throws Exception {
		this.mvc.perform(get("/api/market/drawings?owner=u_ada").header("Authorization", bearer("u_reader")))
			.andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("INVALID_REQUEST"));
		this.mvc.perform(get("/api/market/drawings").header("Authorization", bearer("u_reader")))
			.andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("INVALID_REQUEST"));
	}

	@Test
	void badLimitOrCursorIsRejected() throws Exception {
		owned("u_reader", "&limit=0").andExpect(status().isBadRequest());
		owned("u_reader", "&limit=51").andExpect(status().isBadRequest());
		owned("u_reader", "&cursor=not-a-cursor").andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("INVALID_REQUEST"));
	}

	@Test
	void needsAToken() throws Exception {
		this.mvc.perform(get("/api/market/drawings?owner=me"))
			.andExpect(status().isUnauthorized())
			.andExpect(jsonPath("$.error.code").value("UNAUTHENTICATED"));
	}

	/** Mints a drawing for {@code owner} and backdates its acquisition. */
	private UUID mint(String owner, Instant acquired) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, owner)));
		this.db.sql("UPDATE ownership SET updated_at = ? WHERE drawing_id = ?")
			.params(acquired.atOffset(ZoneOffset.UTC), drawing)
			.update();
		return drawing;
	}

	private ResultActions owned(String user, String query) throws Exception {
		return this.mvc.perform(get("/api/market/drawings?owner=me" + query).header("Authorization", bearer(user)));
	}

	private static String bearer(String user) {
		return "Bearer " + TestTokens.valid(user);
	}

}
