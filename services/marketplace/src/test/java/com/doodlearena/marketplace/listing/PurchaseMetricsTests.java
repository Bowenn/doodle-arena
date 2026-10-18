package com.doodlearena.marketplace.listing;

import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.test.context.bean.override.mockito.MockitoSpyBean;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ownership.MintRequest;
import com.doodlearena.marketplace.ownership.MintService;
import com.doodlearena.marketplace.support.IntegrationTest;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.marketplace.telemetry.MarketMetrics;

import static org.assertj.core.api.Assertions.assertThat;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;

/** market.purchases{result=success} counts sales that committed, not ones rolled back afterwards. */
@IntegrationTest
class PurchaseMetricsTests {

	@MockitoSpyBean
	private MarketMetrics metrics;

	@Autowired
	private PurchaseService purchases;

	@Autowired
	private ListingService listings;

	@Autowired
	private MintService mints;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private TransactionTemplate tx;

	@Test
	void aRolledBackPurchaseIsNotCounted() {
		AccountRef seller = this.fixtures.newUser();
		AccountRef buyer = this.fixtures.newUserWith(100);
		Listing listing = listed(seller);

		// Like a lost idempotency claim: the purchase ran, then its transaction rolled back.
		this.tx.executeWithoutResult((status) -> {
			this.purchases.purchase(buyer.ownerId(), listing.listingId());
			status.setRollbackOnly();
		});
		verify(this.metrics, never()).purchase("success");
		assertThat(this.fixtures.balance(buyer)).isEqualTo(100);

		this.tx.executeWithoutResult((status) -> this.purchases.purchase(buyer.ownerId(), listing.listingId()));
		verify(this.metrics, times(1)).purchase("success");
	}

	private Listing listed(AccountRef seller) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, seller.ownerId())));
		return this.listings.create(seller.ownerId(), drawing, 30);
	}

}
