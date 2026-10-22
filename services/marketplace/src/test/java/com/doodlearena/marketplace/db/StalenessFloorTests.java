package com.doodlearena.marketplace.db;

import java.time.Duration;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;

import org.junit.jupiter.api.Test;

import static org.assertj.core.api.Assertions.assertThat;

/** The bound on how stale a replica read may be (ADR 0020), on a clock the test moves. */
class StalenessFloorTests {

	private final AtomicLong clock = new AtomicLong();

	private final AtomicInteger primaryPosition = new AtomicInteger();

	private final AtomicInteger samples = new AtomicInteger();

	private final StalenessFloor floor = new StalenessFloor(Duration.ofSeconds(5), () -> {
		this.samples.incrementAndGet();
		return "0/" + Integer.toHexString(this.primaryPosition.get());
	}, this.clock::get);

	private void after(long millis, int position) {
		this.clock.addAndGet(Duration.ofMillis(millis).toNanos());
		this.primaryPosition.set(position);
	}

	@Test
	void theFloorIsWhatThePrimaryHadUpToMaxLagAgo() {
		after(0, 0x10);
		assertThat(this.floor.floor()).isEqualTo("0/10");
		for (int second = 1; second <= 5; second++) {
			after(1000, 0x10 + second);
			this.floor.floor();
		}
		// Five seconds on: the oldest sample is still in the window.
		assertThat(this.floor.floor()).isEqualTo("0/10");
		after(1000, 0x20);
		assertThat(this.floor.floor()).isEqualTo("0/11");
	}

	@Test
	void thePrimaryIsAskedAtMostOnceASecond() {
		after(0, 0x10);
		for (int i = 0; i < 100; i++) {
			after(5, 0x10 + i);
			this.floor.floor();
		}
		assertThat(this.samples.get()).isEqualTo(1);
		after(1000, 0x99);
		this.floor.floor();
		assertThat(this.samples.get()).isEqualTo(2);
	}

	@Test
	void afterAQuietSpellTheFloorIsTheNewestPosition() {
		after(0, 0x10);
		this.floor.floor();
		after(60_000, 0x30);
		// Nothing sampled within the last 5 s but now: the replica must have what the primary has.
		assertThat(this.floor.floor()).isEqualTo("0/30");
	}

}
