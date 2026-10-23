package com.doodlearena.marketplace.tip;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.function.Consumer;

import org.junit.jupiter.api.Test;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.test.context.TestPropertySource;

import com.doodlearena.marketplace.ledger.AccountRef;
import com.doodlearena.marketplace.ledger.LedgerService;
import com.doodlearena.marketplace.ledger.Posting;
import com.doodlearena.marketplace.ledger.ReconciliationJob;
import com.doodlearena.marketplace.ledger.TransactionType;
import com.doodlearena.marketplace.support.ApiTestSupport;
import com.doodlearena.marketplace.support.LedgerFixtures;

import static org.assertj.core.api.Assertions.assertThat;

/**
 * The hot wallet, measured (ADR 0021): {@value #TIPS} tips from as many viewers to one drawer, on
 * {@value #THREADS} threads, once posted directly (each tip locks the drawer's balance row) and
 * once through the escrow (each tip locks one of 16 shards; one settlement pays the drawer). Both
 * must move every coin exactly once; the timings are logged for docs/performance.md, not asserted,
 * because they depend on the machine.
 */
@TestPropertySource(properties = "spring.datasource.hikari.maximum-pool-size=" + TipContentionTests.THREADS)
class TipContentionTests extends ApiTestSupport {

	private static final Logger log = LoggerFactory.getLogger(TipContentionTests.class);

	static final int THREADS = 32;

	static final int TIPS = 1_000;

	@Autowired
	private LedgerService ledger;

	@Autowired
	private TipService tips;

	@Autowired
	private TipSettlement settlement;

	@Autowired
	private LedgerFixtures fixtures;

	@Autowired
	private ReconciliationJob reconciliation;

	@Test
	void escrowAndDirectPostingBothMoveEveryCoinOnce() throws Exception {
		AccountRef drawer = this.fixtures.newUser();
		List<AccountRef> viewers = viewers();
		Timing direct = run(viewers, (viewer) -> this.ledger.post(TransactionType.TIP, "direct:" + UUID.randomUUID(),
				Map.of(), List.of(Posting.debit(viewer, 1), Posting.credit(drawer, 1))));
		assertThat(this.fixtures.balance(drawer)).isEqualTo(TIPS);

		AccountRef escrowDrawer = this.fixtures.newUser();
		String match = "m_" + UUID.randomUUID();
		Timing escrow = run(viewers, (viewer) -> this.tips.tip(viewer.ownerId(), "r_1", match, 1, 1));
		long settleStarted = System.nanoTime();
		this.settlement.roundFinished(new RoundRequest(match, "r_1", 1, List.of(escrowDrawer.ownerId())));
		double settleMs = (System.nanoTime() - settleStarted) / 1e6;
		assertThat(this.fixtures.balance(escrowDrawer)).isEqualTo(TIPS);

		for (AccountRef viewer : viewers) {
			assertThat(this.fixtures.balance(viewer)).isZero();
		}
		assertThat(this.reconciliation.reconcile().clean()).isTrue();
		log.atInfo()
			.addKeyValue("tips", TIPS)
			.addKeyValue("threads", THREADS)
			.addKeyValue("direct", direct)
			.addKeyValue("escrow", escrow)
			.addKeyValue("settlement_ms", Math.round(settleMs))
			.log("tip contention measured");
		System.out.printf("tip contention: %d tips, %d threads%n  direct: %s%n  escrow: %s, then one settlement in %.0f ms%n",
				TIPS, THREADS, direct, escrow, settleMs);
	}

	/** Each viewer has 2 coins: one for the direct tip, one for the escrow tip. */
	private List<AccountRef> viewers() {
		List<AccountRef> viewers = new ArrayList<>();
		for (int i = 0; i < TIPS; i++) {
			viewers.add(this.fixtures.newUserWith(2));
		}
		return viewers;
	}

	record Timing(double seconds, double p50Ms, double p99Ms) {

		double perSecond() {
			return TIPS / this.seconds;
		}

		@Override
		public String toString() {
			return "%.0f tips/s, p50 %.1f ms, p99 %.1f ms".formatted(perSecond(), this.p50Ms, this.p99Ms);
		}

	}

	/** Every viewer tips once, spread over the threads; returns throughput and per-tip latency. */
	private Timing run(List<AccountRef> viewers, Consumer<AccountRef> tip) throws Exception {
		ExecutorService pool = Executors.newFixedThreadPool(THREADS);
		double[] latencies = new double[viewers.size()];
		long started = System.nanoTime();
		try {
			List<Future<?>> done = new ArrayList<>();
			for (int i = 0; i < viewers.size(); i++) {
				int n = i;
				done.add(pool.submit(() -> {
					long t = System.nanoTime();
					tip.accept(viewers.get(n));
					latencies[n] = (System.nanoTime() - t) / 1e6;
				}));
			}
			for (Future<?> f : done) {
				f.get();
			}
		}
		finally {
			pool.shutdown();
		}
		double seconds = (System.nanoTime() - started) / 1e9;
		Arrays.sort(latencies);
		return new Timing(seconds, latencies[latencies.length / 2], latencies[(int) (latencies.length * 0.99)]);
	}

}
