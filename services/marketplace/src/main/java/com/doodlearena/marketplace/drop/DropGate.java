package com.doodlearena.marketplace.drop;

import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.function.Supplier;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.metrics.DoubleHistogram;

import org.springframework.core.io.ClassPathResource;
import org.springframework.dao.DataAccessException;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.data.redis.core.script.DefaultRedisScript;
import org.springframework.data.redis.core.script.RedisScript;
import org.springframework.stereotype.Component;

import com.doodlearena.marketplace.telemetry.MarketMetrics;

/**
 * A drop's inventory and waiting room in Redis (ADR 0022), each step one Lua script, so it is
 * atomic without locks. Postgres stays the truth: this only keeps the burst away from it.
 *
 * <p>Every key of a drop shares the hash tag {@code {drop:<id>}}, so a Redis Cluster would keep
 * them on one node, as the lobby's keys do. Any Redis failure becomes
 * {@link DropsUnavailableException}: drops fail closed.
 */
@Component
public class DropGate {

	/** Keys outlive their drop by this much, for late buyers' answers and the sweeper's repairs. */
	static final Duration KEEP_AFTER_CLOSE = Duration.ofDays(7);

	private static final AttributeKey<String> SCRIPT = AttributeKey.stringKey("script");

	/** What a reservation attempt found. */
	public enum Outcome {

		RESERVED, ALREADY_BOUGHT, NOT_JOINED, NOT_ADMITTED, SOLD_OUT, HELD, GO

	}

	/** An outcome with its number: the edition, or the place still ahead in the queue. */
	public record Answer(Outcome outcome, int value) {
	}

	private final StringRedisTemplate redis;

	private final DoubleHistogram duration;

	private final RedisScript<Long> stock = script("stock", Long.class);

	private final RedisScript<Long> restock = script("restock", Long.class);

	private final RedisScript<Long> join = script("join", Long.class);

	@SuppressWarnings("rawtypes")
	private final RedisScript<List> peek = script("peek", List.class);

	@SuppressWarnings("rawtypes")
	private final RedisScript<List> reserve = script("reserve", List.class);

	private final RedisScript<Long> release = script("release", Long.class);

	private final RedisScript<Long> forget = script("forget", Long.class);

	private final RedisScript<Long> paid = script("paid", Long.class);

	private final RedisScript<Long> rate = script("rate", Long.class);

	public DropGate(StringRedisTemplate redis, MarketMetrics metrics) {
		this.redis = redis;
		this.duration = metrics.meter()
			.histogramBuilder("market.drops.reserve.duration")
			.setUnit("ms")
			.setDescription("Time a drop's Redis script took, per script")
			.build();
	}

	private static <T> RedisScript<T> script(String name, Class<T> type) {
		DefaultRedisScript<T> script = new DefaultRedisScript<>();
		script.setLocation(new ClassPathResource("drops/" + name + ".lua"));
		script.setResultType(type);
		return script;
	}

	static String key(UUID dropId, String part) {
		return "market:{drop:" + dropId + "}:" + part;
	}

	/** Places admitted so far: {@code admitPerSecond} for every started second since the open. */
	static long admitted(Drop drop, Instant now) {
		if (now.isBefore(drop.opensAt())) {
			return 0;
		}
		return drop.admitPerSecond() * (Duration.between(drop.opensAt(), now).toSeconds() + 1);
	}

	private static String ttl(Drop drop, Instant now) {
		return Long.toString(Math.max(60, Duration.between(now, drop.closesAt().plus(KEEP_AFTER_CLOSE)).toSeconds()));
	}

	/** Fills a new drop's free list with editions 1..N. False if it was already stocked. */
	public boolean stock(Drop drop, Instant now) {
		return run("stock", () -> this.redis.execute(this.stock,
				List.of(key(drop.dropId(), "free"), key(drop.dropId(), "stocked")), Integer.toString(drop.editions()),
				ttl(drop, now))) == 1;
	}

	/** Whether the drop's inventory is in Redis (false after Redis lost its data). */
	public boolean stocked(Drop drop) {
		return Boolean.TRUE.equals(run("stocked", () -> this.redis.hasKey(key(drop.dropId(), "stocked"))));
	}

	/** Rebuilds the free list from what Postgres sold; -1 if it is stocked after all. */
	public long restock(Drop drop, List<Integer> sold, Instant now) {
		List<String> args = new ArrayList<>(List.of(Integer.toString(drop.editions()), ttl(drop, now)));
		sold.forEach((edition) -> args.add(Integer.toString(edition)));
		return run("restock", () -> this.redis.execute(this.restock,
				List.of(key(drop.dropId(), "free"), key(drop.dropId(), "stocked")), args.toArray(Object[]::new)));
	}

	/** Puts the user in the waiting room with this score (first join wins); returns their rank. */
	public long join(Drop drop, String user, double score, Instant now) {
		return run("join", () -> this.redis.execute(this.join, List.of(key(drop.dropId(), "queue")), user,
				Double.toString(score), ttl(drop, now)));
	}

	/** The user's rank in the waiting room, 0-based; null if they haven't joined. */
	public Long rank(Drop drop, String user) {
		return run("rank", () -> this.redis.opsForZSet().rank(key(drop.dropId(), "queue"), user));
	}

	/** People in the waiting room. */
	public long waiting(Drop drop) {
		Long size = run("waiting", () -> this.redis.opsForZSet().zCard(key(drop.dropId(), "queue")));
		return (size == null) ? 0 : size;
	}

	/** Editions not reserved or sold, as Redis counts them. */
	public long editionsLeft(Drop drop) {
		Long size = run("left", () -> this.redis.opsForList().size(key(drop.dropId(), "free")));
		return (size == null) ? 0 : size;
	}

	/** The refusal a purchase would get, without changing anything; HELD or GO means go on. */
	public Answer peek(Drop drop, String user, Instant now) {
		return answer("peek", () -> this.redis.execute(this.peek, keys(drop), user, Long.toString(admitted(drop, now))));
	}

	/** Takes an edition for an admitted user, or says why not. */
	public Answer reserve(Drop drop, String user, Instant now) {
		return answer("reserve", () -> this.redis.execute(this.reserve, keys(drop), user,
				Long.toString(now.toEpochMilli()), Long.toString(admitted(drop, now)), ttl(drop, now)));
	}

	/** Gives the user's unpaid reservation of this edition back to the free list. */
	public boolean release(Drop drop, String user, int edition) {
		return run("release", () -> this.redis.execute(this.release,
				List.of(key(drop.dropId(), "buyers"), key(drop.dropId(), "free")), user, Integer.toString(edition))) == 1;
	}

	/** Drops the user's reservation without returning the edition (Postgres sold it to someone else). */
	public boolean forget(Drop drop, String user, int edition) {
		return run("forget", () -> this.redis.execute(this.forget, List.of(key(drop.dropId(), "buyers")), user,
				Integer.toString(edition))) == 1;
	}

	/** Marks the user's edition paid, once Postgres committed it. */
	public void paid(Drop drop, String user, int edition, Instant now) {
		run("paid", () -> this.redis.execute(this.paid, List.of(key(drop.dropId(), "buyers")), user,
				Integer.toString(edition), ttl(drop, now)));
	}

	/** Every buyer's entry: "r:&lt;edition&gt;:&lt;reserved at ms&gt;" or "p:&lt;edition&gt;". */
	public Map<String, String> buyers(Drop drop) {
		return run("buyers", () -> this.redis.<String, String>opsForHash().entries(key(drop.dropId(), "buyers")));
	}

	/**
	 * One request against the user's token bucket ({@code perSecond}, at most {@code burst}).
	 * @return 0 when it may go ahead, else the milliseconds until it may
	 */
	public long rateLimit(String user, Instant now, int perSecond, int burst) {
		return run("rate", () -> this.redis.execute(this.rate, List.of("market:rl:" + user),
				Long.toString(now.toEpochMilli()), Integer.toString(perSecond), Integer.toString(burst)));
	}

	private static List<String> keys(Drop drop) {
		return List.of(key(drop.dropId(), "queue"), key(drop.dropId(), "buyers"), key(drop.dropId(), "free"));
	}

	@SuppressWarnings("rawtypes")
	private Answer answer(String script, Supplier<List> call) {
		List result = run(script, call);
		return new Answer(Outcome.valueOf(String.valueOf(result.get(0))), Integer.parseInt(String.valueOf(result.get(1))));
	}

	private <T> T run(String script, Supplier<T> call) {
		long started = System.nanoTime();
		try {
			T result = call.get();
			if (result == null && !script.equals("rank")) {
				throw new DropsUnavailableException(script + " returned nothing", null);
			}
			return result;
		}
		catch (DataAccessException ex) {
			throw new DropsUnavailableException("Redis " + script + " failed", ex);
		}
		finally {
			this.duration.record((System.nanoTime() - started) / 1e6, Attributes.of(SCRIPT, script));
		}
	}

}
