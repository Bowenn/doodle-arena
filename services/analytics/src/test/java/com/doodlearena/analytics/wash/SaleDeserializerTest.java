package com.doodlearena.analytics.wash;

import static org.junit.jupiter.api.Assertions.assertEquals;

import com.doodlearena.proto.doodle.events.v1.SaleCompleted;
import com.doodlearena.proto.doodle.events.v1.SaleEvent;
import com.google.protobuf.Timestamp;
import java.util.ArrayList;
import java.util.List;
import org.apache.flink.util.Collector;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.junit.jupiter.api.Test;

class SaleDeserializerTest {
	private static final Timestamp AT = Timestamp.newBuilder().setSeconds(1_800_000_000L).setNanos(250_000_000).build();

	private static List<Sale> read(byte[] value) {
		List<Sale> out = new ArrayList<>();
		new SaleDeserializer().deserialize(new ConsumerRecord<>("market.sales.v1", 0, 7, new byte[0], value),
				new Collector<>() {
					@Override
					public void collect(Sale s) {
						out.add(s);
					}

					@Override
					public void close() {}
				});
		return out;
	}

	private static SaleCompleted.Builder sale() {
		return SaleCompleted.newBuilder().setSaleId("s1").setDrawingId("d1").setSellerId("alice").setBuyerId("bob")
				.setPrice(120).setCompletedAt(AT);
	}

	private static byte[] event(SaleCompleted.Builder s) {
		return SaleEvent.newBuilder().setSaleCompleted(s).build().toByteArray();
	}

	@Test
	void readsASaleWithItsEventTime() {
		List<Sale> out = read(event(sale()));
		assertEquals(1, out.size());
		Sale s = out.get(0);
		assertEquals("s1", s.saleId);
		assertEquals("d1", s.drawingId);
		assertEquals("alice", s.sellerId);
		assertEquals("bob", s.buyerId);
		assertEquals(120, s.price);
		assertEquals(1_800_000_000_250L, s.completedAtMs);
	}

	// ADR 0004: a record the job can't use is skipped, never a reason to fail.
	@Test
	void skipsWhatItCantUse() {
		assertEquals(0, read(new byte[] {(byte) 0xff, 0x01}).size(), "garbage bytes");
		assertEquals(0, read(SaleEvent.getDefaultInstance().toByteArray()).size(), "no case set");
		assertEquals(0, read(event(sale().clearSaleId())).size(), "no sale id");
		assertEquals(0, read(event(sale().clearDrawingId())).size(), "no drawing");
		assertEquals(0, read(event(sale().clearBuyerId())).size(), "no buyer");
		assertEquals(0, read(event(sale().clearCompletedAt())).size(), "no event time");
	}
}
