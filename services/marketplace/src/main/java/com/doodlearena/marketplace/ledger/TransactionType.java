package com.doodlearena.marketplace.ledger;

/** Why coins moved. Stored as text; migrations V1, V10 and V11 list the allowed values. */
public enum TransactionType {

	REWARD("reward"), STRIPE_PURCHASE("stripe_purchase"), SALE("sale"),

	/** A tipper's coins into escrow (ADR 0021). */
	TIP("tip"),

	/** A round's pending tips out of escrow, to its drawers (and back to any tipper who drew it). */
	TIP_SETTLEMENT("tip_settlement"),

	/** A tip whose round never finished, back to its tipper. */
	TIP_REFUND("tip_refund"),

	/** A buyer's coins into a drop's escrow for one edition (ADR 0022). */
	DROP_PURCHASE("drop_purchase"),

	/** A drop's editions' coins out of escrow, to the drawing's authors. */
	DROP_SETTLEMENT("drop_settlement");

	private final String dbValue;

	TransactionType(String dbValue) {
		this.dbValue = dbValue;
	}

	public String dbValue() {
		return this.dbValue;
	}

	public static TransactionType fromDb(String value) {
		for (TransactionType type : values()) {
			if (type.dbValue.equals(value)) {
				return type;
			}
		}
		throw new IllegalArgumentException("unknown transaction type " + value);
	}

}
