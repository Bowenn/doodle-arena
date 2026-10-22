package com.doodlearena.analytics.wash;

/**
 * A completed sale, as the wash-trading detector reads it. A Flink POJO (public fields, a no-arg
 * constructor), so it lives in keyed state without Kryo.
 */
public class Sale {
	public String saleId;
	public String drawingId;
	public String sellerId;
	public String buyerId;
	/** Coins the buyer paid. */
	public long price;
	/** When the sale committed, by the marketplace's clock (event time), in epoch milliseconds. */
	public long completedAtMs;

	public Sale() {}

	public Sale(String saleId, String drawingId, String sellerId, String buyerId, long price, long completedAtMs) {
		this.saleId = saleId;
		this.drawingId = drawingId;
		this.sellerId = sellerId;
		this.buyerId = buyerId;
		this.price = price;
		this.completedAtMs = completedAtMs;
	}
}
