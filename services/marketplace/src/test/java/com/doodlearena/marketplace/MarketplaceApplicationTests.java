package com.doodlearena.marketplace;

import org.junit.jupiter.api.Test;

import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.context.annotation.Import;

import com.doodlearena.marketplace.support.PostgresTestConfiguration;

@SpringBootTest
@Import(PostgresTestConfiguration.class)
class MarketplaceApplicationTests {

	@Test
	void contextLoads() {
	}

}
