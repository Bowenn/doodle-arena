package com.doodlearena.marketplace.listing;

import java.util.ArrayList;
import java.util.List;
import java.util.Queue;
import java.util.UUID;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

import com.jayway.jsonpath.JsonPath;
import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.MediaType;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.mock.web.MockHttpServletResponse;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.ledger.ReconciliationResult;
import com.doodlearena.marketplace.ownership.MintRequest;
import com.doodlearena.marketplace.ownership.MintService;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;
import com.doodlearena.marketplace.support.TestEvents;
import com.doodlearena.marketplace.support.TestTokens;
import com.doodlearena.proto.doodle.events.v1.SaleEvent;

import static org.assertj.core.api.Assertions.assertThat;
import static org.hamcrest.Matchers.nullValue;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.delete;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.header;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

/** The purchase transaction through the whole HTTP stack (design doc, section 8; M4 exit criteria). */
class PurchaseApiTests extends ApiTestSupport {

	@Autowired
	private MockMvc mvc;

	@Autowired
	private MintService mints;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private ReconciliationJob reconciliation;

	@Autowired
	private JdbcClient db;

	@Test
	void buyerGetsTheDrawingAndTheSellerTheCoins() throws Exception {
		AccountRef seller = this.fixtures.newUser();
		AccountRef buyer = this.fixtures.newUserWith(100);
		UUID drawing = mint(seller);
		String listing = list(seller, drawing, 60);

		purchase(buyer, listing, key()).andExpect(status().isOk())
			.andExpect(jsonPath("$.listing_id").value(listing))
			.andExpect(jsonPath("$.drawing_id").value(drawing.toString()))
			.andExpect(jsonPath("$.seller_id").value(seller.ownerId()))
			.andExpect(jsonPath("$.buyer_id").value(buyer.ownerId()))
			.andExpect(jsonPath("$.price").value(60))
			.andExpect(jsonPath("$.sale_id").isNotEmpty())
			.andExpect(jsonPath("$.completed_at").isNotEmpty());

		assertThat(this.fixtures.balance(buyer)).isEqualTo(40);
		assertThat(this.fixtures.balance(seller)).isEqualTo(60);
		this.mvc.perform(get("/api/market/drawings/" + drawing + "/ownership-history").header("Authorization", bearer(buyer)))
			.andExpect(jsonPath("$.owner_id").value(buyer.ownerId()))
			.andExpect(jsonPath("$.version").value(2))
			.andExpect(jsonPath("$.events[0].price").value(nullValue()))
			.andExpect(jsonPath("$.events[1].kind").value("SALE"))
			.andExpect(jsonPath("$.events[1].from_owner").value(seller.ownerId()))
			.andExpect(jsonPath("$.events[1].price").value(60));
		this.mvc.perform(get("/api/market/listings/" + listing).header("Authorization", bearer(buyer)))
			.andExpect(jsonPath("$.status").value("SOLD"))
			.andExpect(jsonPath("$.buyer_id").value(buyer.ownerId()));

		SaleEvent event = SaleEvent.parseFrom(this.db
			.sql("SELECT payload FROM outbox WHERE topic = 'market.sales.v1' AND key = ?")
			.param(drawing.toString())
			.query(byte[].class)
			.single());
		assertThat(event.getSaleCompleted().getBuyerId()).isEqualTo(buyer.ownerId());
		assertThat(event.getSaleCompleted().getPrice()).isEqualTo(60);
		assertThat(event.getSaleCompleted().getRoyaltiesList()).isEmpty();
		assertThat(event.getSaleCompleted().getMeta().getEventType()).isEqualTo("SaleCompleted");
	}

	/** M4: 50 concurrent buyers produce exactly one new owner and 49 × 409, and the ledger still sums to zero. */
	@Test
	void fiftyConcurrentBuyersOneWinner() throws Exception {
		AccountRef seller = this.fixtures.newUser();
		UUID drawing = mint(seller);
		String listing = list(seller, drawing, 25);
		List<AccountRef> buyers = new ArrayList<>();
		for (int i = 0; i < 50; i++) {
			buyers.add(this.fixtures.newUserWith(100));
		}
		Queue<MockHttpServletResponse> responses = new ConcurrentLinkedQueue<>();
		CountDownLatch start = new CountDownLatch(1);

		try (ExecutorService pool = Executors.newFixedThreadPool(50)) {
			List<Future<?>> futures = new ArrayList<>();
			for (AccountRef buyer : buyers) {
				futures.add(pool.submit(() -> {
					start.await();
					responses.add(purchase(buyer, listing, key()).andReturn().getResponse());
					return null;
				}));
			}
			start.countDown();
			for (Future<?> future : futures) {
				future.get(2, TimeUnit.MINUTES);
			}
		}

		List<MockHttpServletResponse> won = responses.stream().filter((r) -> r.getStatus() == 200).toList();
		List<MockHttpServletResponse> lost = responses.stream().filter((r) -> r.getStatus() == 409).toList();
		assertThat(won).hasSize(1);
		assertThat(lost).hasSize(49)
			.allSatisfy((r) -> assertThat(r.getContentAsString()).contains("LISTING_NOT_AVAILABLE"));

		String winner = JsonPath.read(won.getFirst().getContentAsString(), "$.buyer_id");
		assertThat(owner(drawing)).isEqualTo(winner);
		long paid = buyers.stream().filter((b) -> this.fixtures.balance(b) == 75).count();
		long untouched = buyers.stream().filter((b) -> this.fixtures.balance(b) == 100).count();
		assertThat(paid).isEqualTo(1);
		assertThat(untouched).isEqualTo(49);
		assertThat(this.fixtures.balance(seller)).isEqualTo(25);
		assertThat(sales(drawing)).isEqualTo(1);
		ReconciliationResult result = this.reconciliation.reconcile();
		assertThat(result.imbalance()).isZero();
		assertThat(result.clean()).isTrue();
	}

	/** M4: replaying a purchase request has no extra effect. */
	@Test
	void replayedPurchaseHasNoExtraEffect() throws Exception {
		AccountRef seller = this.fixtures.newUser();
		AccountRef buyer = this.fixtures.newUserWith(100);
		UUID drawing = mint(seller);
		String listing = list(seller, drawing, 30);
		String key = key();

		String first = purchase(buyer, listing, key).andExpect(status().isOk()).andReturn().getResponse().getContentAsString();
		String second = purchase(buyer, listing, key).andExpect(status().isOk())
			.andExpect(header().string("Idempotency-Replayed", "true"))
			.andReturn()
			.getResponse()
			.getContentAsString();

		assertThat(second).isEqualTo(first);
		assertThat(this.fixtures.balance(buyer)).isEqualTo(70);
		assertThat(this.fixtures.balance(seller)).isEqualTo(30);
		assertThat(sales(drawing)).isEqualTo(1);
		// Without the key it is a new request, and the listing is gone.
		purchase(buyer, listing, key()).andExpect(status().isConflict())
			.andExpect(jsonPath("$.error.code").value("LISTING_NOT_AVAILABLE"));
	}

	@Test
	void insufficientFundsIsStoredAndReplayed() throws Exception {
		AccountRef seller = this.fixtures.newUser();
		AccountRef buyer = this.fixtures.newUserWith(10);
		String listing = list(seller, mint(seller), 30);
		String key = key();

		purchase(buyer, listing, key).andExpect(status().isUnprocessableContent())
			.andExpect(jsonPath("$.error.code").value("INSUFFICIENT_FUNDS"));
		this.fixtures.fund(buyer, 100);
		// Same key, same answer, even though the buyer could pay now: a new attempt needs a new key.
		purchase(buyer, listing, key).andExpect(status().isUnprocessableContent())
			.andExpect(header().string("Idempotency-Replayed", "true"));
		assertThat(this.fixtures.balance(buyer)).isEqualTo(110);
		purchase(buyer, listing, key()).andExpect(status().isOk());
	}

	@Test
	void cannotBuyOwnOrMissingListing() throws Exception {
		AccountRef seller = this.fixtures.newUserWith(100);
		String listing = list(seller, mint(seller), 30);

		purchase(seller, listing, key()).andExpect(status().isUnprocessableContent())
			.andExpect(jsonPath("$.error.code").value("CANNOT_BUY_OWN_LISTING"));
		purchase(seller, UUID.randomUUID().toString(), key()).andExpect(status().isNotFound())
			.andExpect(jsonPath("$.error.code").value("LISTING_NOT_FOUND"));
		purchase(seller, "nope", key()).andExpect(status().isNotFound());
		this.mvc.perform(post("/api/market/listings/" + listing + "/purchase").header("Authorization", bearer(seller)))
			.andExpect(status().isBadRequest())
			.andExpect(jsonPath("$.error.code").value("IDEMPOTENCY_KEY_REQUIRED"));
	}

	@Test
	void cancelledListingCannotBeBought() throws Exception {
		AccountRef seller = this.fixtures.newUser();
		AccountRef buyer = this.fixtures.newUserWith(100);
		String listing = list(seller, mint(seller), 30);
		this.mvc.perform(delete("/api/market/listings/" + listing).header("Authorization", bearer(seller)))
			.andExpect(status().isOk());

		purchase(buyer, listing, key()).andExpect(status().isConflict());
		assertThat(this.fixtures.balance(buyer)).isEqualTo(100);
	}

	/** The new owner can sell it on; the history keeps both sales in order. */
	@Test
	void boughtDrawingCanBeResold() throws Exception {
		AccountRef first = this.fixtures.newUser();
		AccountRef second = this.fixtures.newUserWith(100);
		AccountRef third = this.fixtures.newUserWith(100);
		UUID drawing = mint(first);
		purchase(second, list(first, drawing, 10), key()).andExpect(status().isOk());
		purchase(third, list(second, drawing, 40), key()).andExpect(status().isOk());

		assertThat(owner(drawing)).isEqualTo(third.ownerId());
		// The second sale pays the author, who is no longer a party, 10 % of 40.
		assertThat(this.fixtures.balance(second)).isEqualTo(100 - 10 + 36);
		assertThat(this.fixtures.balance(first)).isEqualTo(10 + 4);
		assertThat(this.reconciliation.reconcile().ownershipViolationIds()).doesNotContain(drawing);
	}

	/** Royalties (task 4.6): the authors who are neither the seller nor the buyer share 10 %. */
	@Test
	void coDrawnDrawingPaysItsOtherAuthors() throws Exception {
		AccountRef ada = this.fixtures.newUser();
		AccountRef bob = this.fixtures.newUser();
		AccountRef zed = this.fixtures.newUserWith(200);
		AccountRef yan = this.fixtures.newUserWith(200);
		UUID drawing = mintCoDrawn(ada, bob);

		// Ada, the first author and owner, sells: Bob gets the royalty.
		purchase(zed, list(ada, drawing, 55), key()).andExpect(status().isOk())
			.andExpect(jsonPath("$.royalties.length()").value(1))
			.andExpect(jsonPath("$.royalties[0].user_id").value(bob.ownerId()))
			.andExpect(jsonPath("$.royalties[0].amount").value(5));
		assertThat(this.fixtures.balance(ada)).isEqualTo(50);
		assertThat(this.fixtures.balance(bob)).isEqualTo(5);
		assertThat(this.fixtures.balance(zed)).isEqualTo(145);

		// Zed, not an author, sells it on: both authors share floor(81 / 10) = 8.
		purchase(yan, list(zed, drawing, 81), key()).andExpect(status().isOk());
		assertThat(this.fixtures.balance(zed)).isEqualTo(145 + 73);
		assertThat(this.fixtures.balance(ada)).isEqualTo(54);
		assertThat(this.fixtures.balance(bob)).isEqualTo(9);
		assertThat(this.fixtures.balance(yan)).isEqualTo(119);

		SaleEvent last = SaleEvent.parseFrom(this.db
			.sql("SELECT payload FROM outbox WHERE topic = 'market.sales.v1' AND key = ? ORDER BY created_at DESC LIMIT 1")
			.param(drawing.toString())
			.query(byte[].class)
			.single());
		assertThat(last.getSaleCompleted().getRoyaltiesList())
			.extracting((r) -> r.getUserId() + "=" + r.getAmount())
			.containsExactly(ada.ownerId() + "=4", bob.ownerId() + "=4");
		assertThat(this.reconciliation.reconcile().clean()).isTrue();
	}

	@Test
	void buyerWhoIsAnAuthorEarnsNothing() throws Exception {
		AccountRef ada = this.fixtures.newUser();
		AccountRef bob = this.fixtures.newUserWith(100);
		UUID drawing = mintCoDrawn(ada, bob);

		// Bob buys back the drawing he co-drew from Ada: no royalty to himself, nobody else to pay.
		purchase(bob, list(ada, drawing, 60), key()).andExpect(status().isOk())
			.andExpect(jsonPath("$.royalties").isEmpty());
		assertThat(this.fixtures.balance(bob)).isEqualTo(40);
		assertThat(this.fixtures.balance(ada)).isEqualTo(60);
	}

	private UUID mintCoDrawn(AccountRef first, AccountRef second) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, first.ownerId(), second.ownerId())));
		return drawing;
	}

	private ResultActions purchase(AccountRef buyer, String listing, String key) throws Exception {
		return this.mvc.perform(post("/api/market/listings/" + listing + "/purchase").header("Authorization", bearer(buyer))
			.header("Idempotency-Key", key));
	}

	private String list(AccountRef seller, UUID drawing, long price) throws Exception {
		String body = this.mvc
			.perform(post("/api/market/listings").header("Authorization", bearer(seller))
				.header("Idempotency-Key", key())
				.contentType(MediaType.APPLICATION_JSON)
				.content("{\"drawing_id\":\"" + drawing + "\",\"price\":" + price + "}"))
			.andExpect(status().isCreated())
			.andReturn()
			.getResponse()
			.getContentAsString();
		return JsonPath.read(body, "$.listing_id");
	}

	private UUID mint(AccountRef owner) {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(TestEvents.approved(drawing, owner.ownerId())));
		return drawing;
	}

	private String owner(UUID drawing) {
		return this.db.sql("SELECT owner_id FROM ownership WHERE drawing_id = ?").param(drawing).query(String.class).single();
	}

	private long sales(UUID drawing) {
		return this.db.sql("SELECT COUNT(*) FROM outbox WHERE topic = 'market.sales.v1' AND key = ?")
			.param(drawing.toString())
			.query(Long.class)
			.single();
	}

	private static String key() {
		return UUID.randomUUID().toString();
	}

	private static String bearer(AccountRef user) {
		return "Bearer " + TestTokens.valid(user.ownerId());
	}

}
