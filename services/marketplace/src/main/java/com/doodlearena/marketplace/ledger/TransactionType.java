package com.doodlearena.marketplace.ledger;

/** Why coins moved. Stored as text; migration V1 lists the allowed values. */
public enum TransactionType {

	REWARD("reward"), STRIPE_PURCHASE("stripe_purchase"), SALE("sale");

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
