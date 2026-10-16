package com.doodlearena.marketplace.health;

import org.junit.jupiter.api.Test;

import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;

import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

class HealthControllerTests {

	private final MockMvc mvc = MockMvcBuilders.standaloneSetup(new HealthController("1.2.3", "marketplace-1"))
		.build();

	@Test
	void healthzReturnsServiceIdentity() throws Exception {
		this.mvc.perform(get("/healthz"))
			.andExpect(status().isOk())
			.andExpect(jsonPath("$.status").value("ok"))
			.andExpect(jsonPath("$.service").value("marketplace"))
			.andExpect(jsonPath("$.version").value("1.2.3"))
			.andExpect(jsonPath("$.instance").value("marketplace-1"));
	}

	@Test
	void healthzRejectsPost() throws Exception {
		this.mvc.perform(post("/healthz")).andExpect(status().isMethodNotAllowed());
	}

}
