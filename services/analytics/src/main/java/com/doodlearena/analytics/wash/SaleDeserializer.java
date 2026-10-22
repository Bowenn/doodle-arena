package com.doodlearena.analytics.wash;

import com.doodlearena.proto.doodle.events.v1.SaleCompleted;
import com.doodlearena.proto.doodle.events.v1.SaleEvent;
import com.google.protobuf.InvalidProtocolBufferException;
import org.apache.flink.api.common.typeinfo.TypeInformation;
import org.apache.flink.connector.kafka.source.reader.deserializer.KafkaRecordDeserializationSchema;
import org.apache.flink.metrics.Counter;
import org.apache.flink.util.Collector;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Reads market.sales.v1. An unset or unknown case, bytes that don't decode or a sale without its
 * ids or time is logged with its topic, partition and offset, counted and skipped (ADR 0004): one
 * bad record must never stop the job.
 */
public class SaleDeserializer implements KafkaRecordDeserializationSchema<Sale> {
	private static final Logger LOG = LoggerFactory.getLogger(SaleDeserializer.class);

	private transient Counter read;
	private transient Counter skipped;

	@Override
	public void open(org.apache.flink.api.common.serialization.DeserializationSchema.InitializationContext context) {
		read = context.getMetricGroup().counter("analytics.sales.read");
		skipped = context.getMetricGroup().counter("analytics.sales.skipped");
	}

	@Override
	public void deserialize(ConsumerRecord<byte[], byte[]> record, Collector<Sale> out) {
		SaleCompleted s;
		try {
			SaleEvent e = SaleEvent.parseFrom(record.value());
			if (e.getEventCase() != SaleEvent.EventCase.SALE_COMPLETED) {
				skip(record, "unset or unknown case " + e.getEventCase());
				return;
			}
			s = e.getSaleCompleted();
		} catch (InvalidProtocolBufferException ex) {
			skip(record, "not a SaleEvent: " + ex.getMessage());
			return;
		}
		if (s.getSaleId().isEmpty() || s.getDrawingId().isEmpty() || s.getSellerId().isEmpty()
				|| s.getBuyerId().isEmpty() || !s.hasCompletedAt()) {
			skip(record, "a sale without its ids or time");
			return;
		}
		if (read != null) {
			read.inc();
		}
		long ms = s.getCompletedAt().getSeconds() * 1000 + s.getCompletedAt().getNanos() / 1_000_000;
		out.collect(new Sale(s.getSaleId(), s.getDrawingId(), s.getSellerId(), s.getBuyerId(), s.getPrice(), ms));
	}

	private void skip(ConsumerRecord<byte[], byte[]> record, String why) {
		if (skipped != null) {
			skipped.inc();
		}
		LOG.warn("skipped a record: {} (topic {}, partition {}, offset {})", why, record.topic(), record.partition(),
				record.offset());
	}

	@Override
	public TypeInformation<Sale> getProducedType() {
		return TypeInformation.of(Sale.class);
	}
}
