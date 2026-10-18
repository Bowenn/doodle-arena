package com.doodlearena.marketplace.listing;

import java.util.ArrayList;
import java.util.List;

/**
 * The royalty split (design doc, section 8), in integer coins only.
 *
 * <pre>
 * recipients    = the drawing's authors in drawing order, minus the seller, minus the buyer
 * royalty_total = floor(price * percent / 100)
 * share         = floor(royalty_total / recipients), the remainder to the first recipient
 * seller gets   = price - royalty_total
 * </pre>
 *
 * Nobody earns a royalty on their own sale, and paying yourself one would only be a discount, so
 * the seller and the buyer are never recipients. A recipient whose share comes to 0 is left out, so
 * every ledger entry is non-zero; the shares still add up to royalty_total.
 */
public final class Royalties {

	private Royalties() {
	}

	/** One author's royalty from a sale. */
	public record RoyaltyShare(String userId, long amount) {
	}

	public static List<RoyaltyShare> split(long price, List<String> authors, String sellerId, String buyerId,
			int percent) {
		if (price < 0 || percent < 0 || percent > 100) {
			throw new IllegalArgumentException("price must be >= 0 and percent between 0 and 100");
		}
		List<String> recipients = authors.stream()
			.filter((author) -> !author.equals(sellerId) && !author.equals(buyerId))
			.distinct()
			.toList();
		long total = Math.multiplyExact(price, percent) / 100;
		if (recipients.isEmpty() || total == 0) {
			return List.of();
		}
		long share = total / recipients.size();
		long remainder = total % recipients.size();
		List<RoyaltyShare> shares = new ArrayList<>();
		for (int i = 0; i < recipients.size(); i++) {
			long amount = share + ((i == 0) ? remainder : 0);
			if (amount > 0) {
				shares.add(new RoyaltyShare(recipients.get(i), amount));
			}
		}
		return shares;
	}

	public static long total(List<RoyaltyShare> shares) {
		return shares.stream().mapToLong(RoyaltyShare::amount).sum();
	}

}
