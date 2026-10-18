package com.doodlearena.marketplace.idempotency;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Duration;
import java.util.HexFormat;
import java.util.Map;
import java.util.UUID;
import java.util.function.BiFunction;
import java.util.function.Function;
import java.util.regex.Pattern;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.http.HttpStatus;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Service;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;

import com.doodlearena.marketplace.api.ApiException;
import com.doodlearena.marketplace.api.ErrorBody;
import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * {@code Idempotency-Key} handling for money-moving requests (ADR 0012).
 *
 * <ol>
 * <li><b>Claim</b>, in its own committed transaction: insert the key as IN_PROGRESS with a fresh
 * claim token and a lease. If the key exists: another request body is 422 IDEMPOTENCY_KEY_REUSED;
 * DONE replays the stored answer; IN_PROGRESS within its lease is 409 IDEMPOTENCY_KEY_IN_PROGRESS;
 * IN_PROGRESS past its lease (the request crashed or hangs) is taken over with a new token.</li>
 * <li><b>Call</b> (optional), with no transaction open and no connection held: a call to another
 * system, such as Stripe, whose wait mustn't tie up the connection pool. It must be safe to repeat
 * under the same key (Stripe's idempotency key, derived from ours), since a request that takes the
 * claim over calls again.</li>
 * <li><b>Work</b>, in the operation's own transaction, which ends by completing the key: an UPDATE
 * that matches only this claim's token. If the claim was taken over meanwhile, it matches nothing,
 * and the whole transaction rolls back: the work happens at most once.</li>
 * <li>A business outcome (403, 404, 409 or 422 from the call or the work) is stored the same way,
 * after the work rolled back, and replayed like a success. Anything else releases the claim, so a retry can
 * start at once.</li>
 * </ol>
 */
@Service
public class IdempotencyService {

	private static final Logger log = LoggerFactory.getLogger(IdempotencyService.class);

	private static final Pattern KEY = Pattern.compile("[\\x21-\\x7e]{1,255}");

	private final JdbcClient db;

	private final TransactionTemplate tx;

	private final JsonMapper json;

	private final MarketMetrics metrics;

	private final Duration lease;

	public IdempotencyService(JdbcClient db, PlatformTransactionManager transactions, JsonMapper json,
			MarketMetrics metrics, @Value("${app.idempotency.lease:30s}") Duration lease) {
		this.db = db;
		this.tx = new TransactionTemplate(transactions);
		this.json = json;
		this.metrics = metrics;
		this.lease = lease;
	}

	/**
	 * Runs {@code work} once per key. {@code work} runs inside a transaction that also completes the
	 * key, and returns the success body.
	 * @param requestHash {@link #hash} of the request, so the same key can't be reused for another
	 */
	public IdempotentResponse execute(String userId, String key, String operation, String requestHash, String requestId,
			HttpStatus successStatus, Function<Claim, Object> work) {
		return execute(userId, key, operation, requestHash, requestId, successStatus, (claim) -> null,
				(claim, nothing) -> work.apply(claim));
	}

	/**
	 * Like {@link #execute(String, String, String, String, String, HttpStatus, Function)}, with a
	 * {@code call} that runs after the claim and outside any transaction; its result is passed to
	 * {@code work}, which runs inside the transaction that completes the key.
	 */
	public <T> IdempotentResponse execute(String userId, String key, String operation, String requestHash,
			String requestId, HttpStatus successStatus, Function<Claim, T> call, BiFunction<Claim, T, Object> work) {
		if (key == null || !KEY.matcher(key).matches()) {
			throw new ApiException(HttpStatus.BAD_REQUEST, "IDEMPOTENCY_KEY_REQUIRED",
					"an Idempotency-Key header of 1 to 255 printable characters is required");
		}
		Object claimed = this.tx.execute((status) -> claim(userId, key, operation, requestHash));
		if (claimed instanceof IdempotentResponse replay) {
			this.metrics.idempotencyReplay(operation);
			log.atInfo().addKeyValue("user_id", userId).addKeyValue("operation", operation).log("idempotent replay");
			return replay;
		}
		Claim claim = (Claim) claimed;
		try {
			T called = call.apply(claim);
			String body = this.tx.execute((status) -> {
				String text = this.json.writeValueAsString(work.apply(claim, called));
				complete(claim, successStatus.value(), text);
				return text;
			});
			return new IdempotentResponse(successStatus.value(), body, false);
		}
		catch (ApiException ex) {
			if (!isBusinessOutcome(ex.status())) {
				release(claim);
				throw ex;
			}
			String body = this.json.writeValueAsString(ErrorBody.of(ex.code(), ex.getMessage(), requestId));
			try {
				this.tx.executeWithoutResult((status) -> complete(claim, ex.status().value(), body));
			}
			catch (ClaimLostException lost) {
				// Taken over while we worked: the request that holds the key now decides the stored answer.
			}
			return new IdempotentResponse(ex.status().value(), body, false);
		}
		catch (ClaimLostException ex) {
			log.atWarn().addKeyValue("user_id", userId).addKeyValue("operation", operation)
				.log("idempotency claim was taken over; this request's work rolled back");
			throw inProgress();
		}
		catch (RuntimeException ex) {
			release(claim);
			throw ex;
		}
	}

	/** SHA-256 of the parts that make two requests "the same": method, path and body. */
	public static String hash(String method, String path, String body) {
		try {
			MessageDigest digest = MessageDigest.getInstance("SHA-256");
			digest.update((method + " " + path + "\n" + (body == null ? "" : body)).getBytes(StandardCharsets.UTF_8));
			return HexFormat.of().formatHex(digest.digest());
		}
		catch (NoSuchAlgorithmException ex) {
			throw new IllegalStateException(ex);
		}
	}

	/** Deletes keys older than 24 hours (design doc, section 8), except a claim still within its lease. */
	@Scheduled(fixedDelayString = "${app.idempotency.cleanup-interval:1h}")
	public int deleteExpiredKeys() {
		int deleted = this.db.sql("""
				DELETE FROM idempotency_keys
				WHERE created_at < now() - interval '24 hours' AND (status = 'DONE' OR locked_until < now())""")
			.update();
		if (deleted > 0) {
			log.atInfo().addKeyValue("deleted", deleted).log("old idempotency keys deleted");
		}
		return deleted;
	}

	private Object claim(String userId, String key, String operation, String requestHash) {
		UUID token = UUID.randomUUID();
		// A concurrent first request with the same key waits here until this insert commits.
		boolean inserted = this.db.sql("""
				INSERT INTO idempotency_keys (user_id, key, operation, request_hash, status, claim_token, locked_until)
				VALUES (?, ?, ?, ?, 'IN_PROGRESS', ?, now() + make_interval(secs => ?))
				ON CONFLICT (user_id, key) DO NOTHING
				RETURNING claim_token""")
			.params(userId, key, operation, requestHash, token, this.lease.toMillis() / 1000.0)
			.query()
			.listOfRows()
			.size() == 1;
		if (inserted) {
			return new Claim(userId, key, token);
		}
		Map<String, Object> row = this.db.sql("""
				SELECT operation, request_hash, status, locked_until < now() AS expired,
					response_code, response_body
				FROM idempotency_keys WHERE user_id = ? AND key = ? FOR UPDATE""")
			.params(userId, key)
			.query()
			.listOfRows()
			.stream()
			.findFirst()
			// Released between the insert and here: claim it again.
			.orElse(null);
		if (row == null) {
			return claim(userId, key, operation, requestHash);
		}
		if (!operation.equals(row.get("operation")) || !requestHash.equals(row.get("request_hash"))) {
			throw new ApiException(HttpStatus.UNPROCESSABLE_CONTENT, "IDEMPOTENCY_KEY_REUSED",
					"this Idempotency-Key was used for a different request");
		}
		if ("DONE".equals(row.get("status"))) {
			return new IdempotentResponse((Integer) row.get("response_code"), (String) row.get("response_body"), true);
		}
		if (!Boolean.TRUE.equals(row.get("expired"))) {
			throw inProgress();
		}
		this.db.sql("""
				UPDATE idempotency_keys SET claim_token = ?, locked_until = now() + make_interval(secs => ?)
				WHERE user_id = ? AND key = ?""")
			.params(token, this.lease.toMillis() / 1000.0, userId, key)
			.update();
		log.atWarn().addKeyValue("user_id", userId).addKeyValue("operation", operation)
			.log("expired idempotency claim taken over");
		return new Claim(userId, key, token);
	}

	/** Completes the key in the caller's transaction; throws if this claim no longer holds it. */
	private void complete(Claim claim, int status, String body) {
		int updated = this.db.sql("""
				UPDATE idempotency_keys
				SET status = 'DONE', response_code = ?, response_body = ?, completed_at = now()
				WHERE user_id = ? AND key = ? AND claim_token = ? AND status = 'IN_PROGRESS'""")
			.params(status, body, claim.userId(), claim.key(), claim.token())
			.update();
		if (updated != 1) {
			throw new ClaimLostException();
		}
	}

	private void release(Claim claim) {
		try {
			this.db.sql("DELETE FROM idempotency_keys WHERE user_id = ? AND key = ? AND claim_token = ? AND status = 'IN_PROGRESS'")
				.params(claim.userId(), claim.key(), claim.token())
				.update();
		}
		catch (RuntimeException ex) {
			// The lease expires on its own.
			log.atWarn().setCause(ex).log("could not release an idempotency claim");
		}
	}

	private static boolean isBusinessOutcome(HttpStatus status) {
		return status == HttpStatus.FORBIDDEN || status == HttpStatus.NOT_FOUND || status == HttpStatus.CONFLICT
				|| status == HttpStatus.UNPROCESSABLE_CONTENT;
	}

	private static ApiException inProgress() {
		return new ApiException(HttpStatus.CONFLICT, "IDEMPOTENCY_KEY_IN_PROGRESS",
				"a request with this Idempotency-Key is still being processed");
	}

	/** The claim's lease expired and another request took the key over. */
	static final class ClaimLostException extends RuntimeException {

		private static final long serialVersionUID = 1L;

		ClaimLostException() {
			super("idempotency claim lost");
		}

	}

}
