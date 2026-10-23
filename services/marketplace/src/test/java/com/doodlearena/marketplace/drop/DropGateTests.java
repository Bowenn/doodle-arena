package com.doodlearena.marketplace.drop;

import java.time.Duration;
import java.time.Instant;
import java.util.UUID;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.data.redis.core.StringRedisTemplate;

import com.doodlearena.marketplace.drop.DropGate.Answer;
import com.doodlearena.marketplace.drop.DropGate.Outcome;
import com.doodlearena.marketplace.support.DropTestSupport;

import static org.assertj.core.api.Assertions.assertThat;

/** The waiting room and the inventory scripts, on a clock the test sets (ADR 0022). */
class DropGateTests extends DropTestSupport {

	private static final Instant OPENS = Instant.parse("2026-11-01T18:00:00Z");

	@Autowired
	private DropGate gate;

	@Autowired
	private StringRedisTemplate redis;

	private static Drop drop(int editions, int admitPerSecond) {
		return new Drop(UUID.randomUUID(), UUID.randomUUID(), editions, 10, OPENS, OPENS.plus(Duration.ofHours(1)),
				admitPerSecond, OPENS.minus(Duration.ofDays(1)));
	}

	@Test
	void beforeTheOpenTheLotteryDecidesAndLaterJoinersQueueBehind() {
		Drop drop = drop(5, 2);
		Instant early = OPENS.minusSeconds(60);
		// Joining first gives no advantage: the scores decide.
		this.gate.join(drop, "u_first", 0.9, early);
		this.gate.join(drop, "u_second", 0.1, early.plusSeconds(1));
		this.gate.join(drop, "u_third", 0.5, early.plusSeconds(2));
		// A second join keeps the first place.
		this.gate.join(drop, "u_first", 0.01, early.plusSeconds(3));
		this.gate.join(drop, "u_late", 1 + OPENS.plusSeconds(5).toEpochMilli(), OPENS.plusSeconds(5));

		assertThat(this.gate.rank(drop, "u_second")).isZero();
		assertThat(this.gate.rank(drop, "u_third")).isEqualTo(1);
		assertThat(this.gate.rank(drop, "u_first")).isEqualTo(2);
		assertThat(this.gate.rank(drop, "u_late")).isEqualTo(3);
		assertThat(this.gate.rank(drop, "u_nobody")).isNull();
		assertThat(this.gate.waiting(drop)).isEqualTo(4);
	}

	@Test
	void placesAreAdmittedAtTheRateForEveryStartedSecond() {
		Drop drop = drop(5, 2);
		assertThat(DropGate.admitted(drop, OPENS.minusMillis(1))).isZero();
		assertThat(DropGate.admitted(drop, OPENS)).isEqualTo(2);
		assertThat(DropGate.admitted(drop, OPENS.plusMillis(999))).isEqualTo(2);
		assertThat(DropGate.admitted(drop, OPENS.plusSeconds(1))).isEqualTo(4);
		assertThat(DropGate.admitted(drop, OPENS.plusSeconds(10))).isEqualTo(22);
	}

	@Test
	void anAdmittedUserReservesOneEditionAndOnlyTheirOwnIsReleased() {
		Drop drop = drop(2, 1);
		this.gate.stock(drop, OPENS);
		for (String user : new String[] { "u_a", "u_b", "u_c" }) {
			this.gate.join(drop, user, this.gate.waiting(drop) / 10.0, OPENS.minusSeconds(10));
		}

		assertThat(this.gate.reserve(drop, "u_b", OPENS)).isEqualTo(new Answer(Outcome.NOT_ADMITTED, 1));
		assertThat(this.gate.peek(drop, "u_b", OPENS)).isEqualTo(new Answer(Outcome.NOT_ADMITTED, 1));
		assertThat(this.gate.peek(drop, "u_z", OPENS)).isEqualTo(new Answer(Outcome.NOT_JOINED, 0));
		Answer a = this.gate.reserve(drop, "u_a", OPENS);
		assertThat(a).isEqualTo(new Answer(Outcome.RESERVED, 1));
		// Asking again returns the same reservation, not a second edition.
		assertThat(this.gate.reserve(drop, "u_a", OPENS)).isEqualTo(a);
		assertThat(this.gate.reserve(drop, "u_b", OPENS.plusSeconds(1))).isEqualTo(new Answer(Outcome.RESERVED, 2));
		assertThat(this.gate.reserve(drop, "u_c", OPENS.plusSeconds(2))).isEqualTo(new Answer(Outcome.SOLD_OUT, 0));
		assertThat(this.gate.peek(drop, "u_c", OPENS.plusSeconds(2))).isEqualTo(new Answer(Outcome.SOLD_OUT, 0));
		// Sold out is told to everyone at once, joined or not.
		assertThat(this.gate.peek(drop, "u_z", OPENS.plusSeconds(2))).isEqualTo(new Answer(Outcome.SOLD_OUT, 0));

		// Someone else's reservation, or the wrong edition, stays.
		assertThat(this.gate.release(drop, "u_c", 1)).isFalse();
		assertThat(this.gate.release(drop, "u_a", 2)).isFalse();
		assertThat(this.gate.release(drop, "u_a", 1)).isTrue();
		assertThat(this.gate.reserve(drop, "u_c", OPENS.plusSeconds(2))).isEqualTo(new Answer(Outcome.RESERVED, 1));

		// Paid can't be released, and a second reservation answers ALREADY_BOUGHT.
		this.gate.paid(drop, "u_b", 2, OPENS);
		assertThat(this.gate.release(drop, "u_b", 2)).isFalse();
		assertThat(this.gate.reserve(drop, "u_b", OPENS.plusSeconds(3))).isEqualTo(new Answer(Outcome.ALREADY_BOUGHT, 2));
		assertThat(this.gate.peek(drop, "u_b", OPENS.plusSeconds(3)).outcome()).isEqualTo(Outcome.HELD);
	}

	@Test
	void stockingTwiceAddsNothingAndRestockSkipsWhatPostgresSold() {
		Drop drop = drop(5, 10);
		assertThat(this.gate.stock(drop, OPENS)).isTrue();
		assertThat(this.gate.stock(drop, OPENS)).isFalse();
		assertThat(this.gate.editionsLeft(drop)).isEqualTo(5);

		this.redis.delete(java.util.List.of(DropGate.key(drop.dropId(), "free"), DropGate.key(drop.dropId(), "stocked")));
		assertThat(this.gate.stocked(drop)).isFalse();
		assertThat(this.gate.restock(drop, java.util.List.of(2, 4), OPENS)).isEqualTo(3);
		assertThat(this.redis.opsForList().range(DropGate.key(drop.dropId(), "free"), 0, -1)).containsExactly("1", "3", "5");
		assertThat(this.gate.restock(drop, java.util.List.of(), OPENS)).isEqualTo(-1);
	}

	@Test
	void theTokenBucketRefillsAtItsRate() {
		String user = "u_" + UUID.randomUUID();
		Instant now = OPENS;
		for (int i = 0; i < 3; i++) {
			assertThat(this.gate.rateLimit(user, now, 2, 3)).isZero();
		}
		assertThat(this.gate.rateLimit(user, now, 2, 3)).isEqualTo(500);
		// Half a second later one token is back.
		assertThat(this.gate.rateLimit(user, now.plusMillis(500), 2, 3)).isZero();
		assertThat(this.gate.rateLimit(user, now.plusMillis(500), 2, 3)).isPositive();
	}

}
