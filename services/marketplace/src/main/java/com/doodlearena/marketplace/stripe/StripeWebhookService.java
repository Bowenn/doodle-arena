package com.doodlearena.marketplace.stripe;

import java.time.Instant;
import java.util.List;
import java.util.Map;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.PostResult;
import com.doodlearena.marketplace.ledger.Posting;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.outbox.OutboxWriter;
import com.doodlearena.proto.doodle.events.v1.CoinsCredited;
import com.doodlearena.proto.doodle.events.v1.CreditReason;
import com.doodlearena.proto.doodle.events.v1.EventMeta;
import com.doodlearena.proto.doodle.events.v1.WalletEvent;

/**
 * Handles a verified Stripe event in one transaction (design doc, section 7.7). Two guards make
 * crediting happen once: the event id (a redelivered event finds its row in stripe_events) and the
 * ledger key {@code stripe:<session_id>} (a second event about the same paid session, such as
 * async_payment_succeeded after completed, finds the ledger transaction). The coins come from our
 * own stripe_checkouts row, never from the event.
 */
@Service
public class StripeWebhookService {

	static final String COMPLETED = "checkout.session.completed";

	static final String ASYNC_SUCCEEDED = "checkout.session.async_payment_succeeded";

	private static final Logger log = LoggerFactory.getLogger(StripeWebhookService.class);

	private final JdbcClient db;

	private final LedgerService ledger;

	private final OutboxWriter outbox;

	public StripeWebhookService(JdbcClient db, LedgerService ledger, OutboxWriter outbox) {
		this.db = db;
		this.ledger = ledger;
		this.outbox = outbox;
	}

	/** What Stripe said, after the signature was checked. */
	public record VerifiedEvent(String eventId, String type, String sessionId, String paymentStatus,
			String metadataUserId) {
	}

	/** processed, duplicate (the event or the session was handled before), ignored or unknown_session. */
	@Transactional
	public String handle(VerifiedEvent event) {
		boolean first = this.db.sql("""
				INSERT INTO stripe_events (stripe_event_id, type, session_id, result) VALUES (?, ?, ?, 'received')
				ON CONFLICT (stripe_event_id) DO NOTHING RETURNING stripe_event_id""")
			.params(event.eventId(), event.type(), event.sessionId())
			.query()
			.listOfRows()
			.size() == 1;
		if (!first) {
			log.atInfo().addKeyValue("stripe_event_id", event.eventId()).log("Stripe event already handled");
			return "duplicate";
		}
		String result = credit(event);
		this.db.sql("UPDATE stripe_events SET result = ? WHERE stripe_event_id = ?").params(result, event.eventId()).update();
		return result;
	}

	private String credit(VerifiedEvent event) {
		boolean paid = (COMPLETED.equals(event.type()) && "paid".equals(event.paymentStatus()))
				|| ASYNC_SUCCEEDED.equals(event.type());
		if (!paid || event.sessionId() == null) {
			return "ignored";
		}
		Map<String, Object> checkout = this.db.sql("SELECT user_id, pack_id, coins FROM stripe_checkouts WHERE session_id = ?")
			.param(event.sessionId())
			.query()
			.listOfRows()
			.stream()
			.findFirst()
			.orElse(null);
		if (checkout == null || !checkout.get("user_id").equals(event.metadataUserId())) {
			// Answered 2xx anyway: Stripe would retry for days, and retrying can't fix it.
			log.atError()
				.addKeyValue("stripe_event_id", event.eventId())
				.addKeyValue("stripe_session_id", event.sessionId())
				.log("paid Stripe session we didn't create, or for another user; not credited");
			return "unknown_session";
		}
		String userId = (String) checkout.get("user_id");
		long coins = (Long) checkout.get("coins");
		PostResult payment = this.ledger.post(TransactionType.STRIPE_PURCHASE, "stripe:" + event.sessionId(),
				Map.of("stripe_session_id", event.sessionId(), "pack_id", (String) checkout.get("pack_id")),
				List.of(Posting.debit(AccountRef.STRIPE_CLEARING, coins), Posting.credit(AccountRef.user(userId), coins)));
		if (payment.replayed()) {
			log.atInfo()
				.addKeyValue("stripe_session_id", event.sessionId())
				.addKeyValue("user_id", userId)
				.log("Stripe session already credited");
			return "duplicate";
		}
		EventMeta meta = OutboxWriter.meta("CoinsCredited", Instant.now());
		this.outbox.write(OutboxWriter.WALLET_TOPIC, userId, meta, WalletEvent.newBuilder()
			.setCoinsCredited(CoinsCredited.newBuilder()
				.setMeta(meta)
				.setUserId(userId)
				.setAmount(coins)
				.setReason(CreditReason.CREDIT_REASON_STRIPE_PURCHASE)
				.setReference(event.sessionId()))
			.build()
			.toByteArray());
		log.atInfo()
			.addKeyValue("user_id", userId)
			.addKeyValue("stripe_session_id", event.sessionId())
			.addKeyValue("coins", coins)
			.log("coin pack credited");
		return "processed";
	}

}
