package com.doodlearena.marketplace.listing;

import java.util.ArrayList;
import java.util.List;
import java.util.Random;

import org.junit.jupiter.api.Test;

import com.doodlearena.marketplace.listing.Royalties.RoyaltyShare;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

/** M4 exit criterion: the royalty rounding cases (design doc, section 8). */
class RoyaltiesTests {

	private static final int PERCENT = 10;

	@Test
	void oneAuthorWhoIsNotTheSellerGetsTenPercent() {
		assertThat(split(100, List.of("ada"), "zed", "yan")).containsExactly(share("ada", 10));
	}

	@Test
	void sharesAreFlooredAndTheRemainderGoesToTheFirstAuthor() {
		assertThat(split(100, List.of("ada", "bob", "cyd"), "zed", "yan"))
			.containsExactly(share("ada", 4), share("bob", 3), share("cyd", 3));
		assertThat(split(1_000_000, List.of("a", "b", "c", "d", "e", "f", "g"), "zed", "yan"))
			// 100,000 / 7 = 14,285 remainder 5.
			.containsExactly(share("a", 14_290), share("b", 14_285), share("c", 14_285), share("d", 14_285),
					share("e", 14_285), share("f", 14_285), share("g", 14_285));
	}

	@Test
	void theTotalIsFlooredToo() {
		// 10 % of 9 is 0.9: no royalty at all.
		assertThat(split(9, List.of("ada"), "zed", "yan")).isEmpty();
		// 10 % of 19 is 1.9, so 1.
		assertThat(split(19, List.of("ada", "bob"), "zed", "yan")).containsExactly(share("ada", 1));
	}

	@Test
	void zeroSharesAreLeftOut() {
		// 1 coin between three authors: the first gets it (the remainder), the others' 0 is left out.
		assertThat(split(10, List.of("ada", "bob", "cyd"), "zed", "yan")).containsExactly(share("ada", 1));
		// 2 coins between three: 0 each plus a remainder of 2 to the first.
		assertThat(split(29, List.of("ada", "bob", "cyd"), "zed", "yan")).containsExactly(share("ada", 2));
	}

	@Test
	void theSellerNeverEarnsARoyalty() {
		assertThat(split(100, List.of("ada"), "ada", "yan")).isEmpty();
		// Co-author Ada sells their pair drawing: Bob gets the whole 10 %.
		assertThat(split(100, List.of("ada", "bob"), "ada", "yan")).containsExactly(share("bob", 10));
	}

	@Test
	void theBuyerNeverEarnsARoyalty() {
		assertThat(split(100, List.of("ada", "bob"), "zed", "bob")).containsExactly(share("ada", 10));
		// The two co-authors trade between themselves: nobody else to pay.
		assertThat(split(100, List.of("ada", "bob"), "ada", "bob")).isEmpty();
	}

	@Test
	void authorOrderDecidesWhoGetsTheRemainder() {
		assertThat(split(100, List.of("cyd", "ada", "bob"), "zed", "yan")).first().isEqualTo(share("cyd", 4));
		// With the first author selling, the remainder goes to the first of the others.
		assertThat(split(110, List.of("ada", "bob", "cyd", "dee"), "ada", "yan"))
			.containsExactly(share("bob", 5), share("cyd", 3), share("dee", 3));
	}

	@Test
	void badInputIsRefused() {
		assertThatThrownBy(() -> split(-1, List.of("ada"), "zed", "yan")).isInstanceOf(IllegalArgumentException.class);
		assertThatThrownBy(() -> Royalties.split(100, List.of("ada"), "zed", "yan", 101))
			.isInstanceOf(IllegalArgumentException.class);
	}

	/** The invariants, over many random sales: nothing is created or lost by rounding. */
	@Test
	void randomSalesAlwaysAddUp() {
		Random random = new Random(42);
		String[] people = { "a", "b", "c", "d", "e", "f", "g", "h", "seller", "buyer" };
		for (int i = 0; i < 20_000; i++) {
			long price = 1 + random.nextInt(1_000_000);
			List<String> authors = new ArrayList<>();
			int count = 1 + random.nextInt(4);
			while (authors.size() < count) {
				String author = people[random.nextInt(people.length)];
				if (!authors.contains(author)) {
					authors.add(author);
				}
			}
			List<RoyaltyShare> shares = split(price, authors, "seller", "buyer");
			long total = Royalties.total(shares);
			long sellerGets = price - total;

			assertThat(shares).allSatisfy((s) -> assertThat(s.amount()).isPositive());
			assertThat(shares).extracting(RoyaltyShare::userId).doesNotContain("seller", "buyer");
			assertThat(sellerGets + total).isEqualTo(price);
			boolean anyRecipient = authors.stream().anyMatch((a) -> !a.equals("seller") && !a.equals("buyer"));
			assertThat(total).isEqualTo(anyRecipient ? price / 10 : 0);
			assertThat(sellerGets).isPositive();
		}
	}

	private static List<RoyaltyShare> split(long price, List<String> authors, String seller, String buyer) {
		return Royalties.split(price, authors, seller, buyer, PERCENT);
	}

	private static RoyaltyShare share(String user, long amount) {
		return new RoyaltyShare(user, amount);
	}

}
