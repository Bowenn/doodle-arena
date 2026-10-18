package com.doodlearena.marketplace.ownership;

import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.marketplace.support.TestTokens;

import static org.hamcrest.Matchers.nullValue;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

class OwnershipApiTests extends ApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private MintService mints;

	@Autowired
	private JdbcClient db;

	@Autowired
	private TransactionTemplate tx;

	@Test
	void historyOfAMintedDrawing() throws Exception {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, "u_ada", "u_bob")));

		history(drawing.toString()).andExpect(status().isOk())
			.andExpect(jsonPath("$.drawing_id").value(drawing.toString()))
			.andExpect(jsonPath("$.owner_id").value("u_ada"))
			.andExpect(jsonPath("$.version").value(1))
			.andExpect(jsonPath("$.author_ids[0]").value("u_ada"))
			.andExpect(jsonPath("$.author_ids[1]").value("u_bob"))
			.andExpect(jsonPath("$.events.length()").value(1))
			.andExpect(jsonPath("$.events[0].kind").value("MINT"))
			.andExpect(jsonPath("$.events[0].from_owner").value(nullValue()))
			.andExpect(jsonPath("$.events[0].to_owner").value("u_ada"))
			.andExpect(jsonPath("$.events[0].sale_id").value(nullValue()))
			.andExpect(jsonPath("$.events[0].created_at").isNotEmpty());
	}

	@Test
	void historyListsEventsOldestFirst() throws Exception {
		UUID drawing = UUID.randomUUID();
		UUID sale = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, "u_cyd")));
		// A sale, written by hand until task 4.3 writes it.
		this.tx.executeWithoutResult((status) -> {
			this.db.sql("UPDATE ownership SET owner_id = 'u_dee', version = version + 1 WHERE drawing_id = ?")
				.param(drawing)
				.update();
			this.db.sql("""
					INSERT INTO ownership_events (drawing_id, kind, from_owner, to_owner, sale_id)
					VALUES (?, 'SALE', 'u_cyd', 'u_dee', ?)""").params(drawing, sale).update();
		});

		history(drawing.toString()).andExpect(status().isOk())
			.andExpect(jsonPath("$.owner_id").value("u_dee"))
			.andExpect(jsonPath("$.version").value(2))
			.andExpect(jsonPath("$.events[0].kind").value("MINT"))
			.andExpect(jsonPath("$.events[1].kind").value("SALE"))
			.andExpect(jsonPath("$.events[1].from_owner").value("u_cyd"))
			.andExpect(jsonPath("$.events[1].to_owner").value("u_dee"))
			.andExpect(jsonPath("$.events[1].sale_id").value(sale.toString()));
	}

	@Test
	void unknownOrMalformedDrawingIsNotFound() throws Exception {
		history(UUID.randomUUID().toString()).andExpect(status().isNotFound())
			.andExpect(jsonPath("$.error.code").value("DRAWING_NOT_FOUND"));
		history("not-a-uuid").andExpect(status().isNotFound())
			.andExpect(jsonPath("$.error.code").value("DRAWING_NOT_FOUND"));
	}

	@Test
	void historyNeedsAToken() throws Exception {
		this.mvc.perform(get("/api/market/drawings/" + UUID.randomUUID() + "/ownership-history"))
			.andExpect(status().isUnauthorized())
			.andExpect(jsonPath("$.error.code").value("UNAUTHENTICATED"));
	}

	private ResultActions history(String id) throws Exception {
		return this.mvc.perform(get("/api/market/drawings/" + id + "/ownership-history").header("Authorization",
				"Bearer " + TestTokens.valid("u_reader")));
	}

}
