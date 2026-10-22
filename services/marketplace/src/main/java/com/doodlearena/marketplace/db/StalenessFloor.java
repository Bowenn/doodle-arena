package com.doodlearena.marketplace.db;

import java.time.Duration;
import java.util.ArrayDeque;
import java.util.function.LongSupplier;
import java.util.function.Supplier;

/**
 * How stale a replica read may be (ADR 0020): the primary's WAL positions over the last
 * {@code maxLag}, sampled at most once a second as reads come in. A replica read needs the oldest
 * of them replayed. Every sample in the window is at most {@code maxLag} old, so a replica past it
 * is at most {@code maxLag} behind, whether it is slow, paused or cut off from the primary. With
 * few reads the window holds only a recent sample, which is stricter, never looser.
 */
final class StalenessFloor {

	private static final long SAMPLE_EVERY = Duration.ofSeconds(1).toNanos();

	private record Sample(long at, String position) {
	}

	private final long maxLag;

	private final Supplier<String> primaryPosition;

	private final LongSupplier nanos;

	private final ArrayDeque<Sample> samples = new ArrayDeque<>();

	StalenessFloor(Duration maxLag, Supplier<String> primaryPosition, LongSupplier nanos) {
		// At least the sampling interval, so the newest sample always stays in the window.
		this.maxLag = Math.max(maxLag.toNanos(), SAMPLE_EVERY);
		this.primaryPosition = primaryPosition;
		this.nanos = nanos;
	}

	/** The position a replica must have replayed to answer a read now. */
	synchronized String floor() {
		long now = this.nanos.getAsLong();
		Sample newest = this.samples.peekLast();
		if (newest == null || now - newest.at() >= SAMPLE_EVERY) {
			this.samples.addLast(new Sample(now, this.primaryPosition.get()));
		}
		while (now - this.samples.getFirst().at() > this.maxLag) {
			this.samples.removeFirst();
		}
		return this.samples.getFirst().position();
	}

}
