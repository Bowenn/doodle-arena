package com.doodlearena.analytics.wash;

import java.time.Duration;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import org.apache.flink.api.common.functions.OpenContext;
import org.apache.flink.api.common.state.ListState;
import org.apache.flink.api.common.state.ListStateDescriptor;
import org.apache.flink.metrics.Counter;
import org.apache.flink.streaming.api.functions.KeyedProcessFunction;
import org.apache.flink.util.Collector;

/**
 * Wash-trading detection (ADR 0018, task 6.5b), keyed by drawing_id: a sale whose buyer sold this
 * same drawing within the window (7 days) closes a loop, and the loop is flagged.
 *
 * <ul>
 *   <li>R1, round trip: two sales, A to B and back to A.
 *   <li>R3, cycle: three or more, A to B to C (and on) back to A, so no two of the accounts
 *       trade directly twice and a rule watching pairs would miss it.
 * </ul>
 *
 * <p>State is the drawing's sales of the last window, oldest first. A drawing's sales come in
 * order: they share a partition (the key is drawing_id), and the marketplace serializes them
 * (whoever owns the drawing is the only possible seller). Since ownership passes from each buyer
 * to the next seller, the sales from the one the closing buyer made up to the closing sale are the
 * loop. The shortest loop is flagged; a drawing that keeps going round gets one flag per sale.
 *
 * <p>Whether a sale is inside the window is decided from the sales' own times, never from the
 * watermark, so the result doesn't depend on how far event time has advanced. Event-time timers
 * only drop the old sales from state; with quiet partitions they may fire late, which costs memory,
 * never a wrong flag.
 */
public class WashTradeDetector extends KeyedProcessFunction<String, Sale, Flag> {
	/**
	 * A drawing traded this often in a window is a bot or a test; past it the oldest sales are
	 * dropped, so one drawing's state stays bounded.
	 */
	static final int MAX_SALES = 200;

	private final long windowMs;
	private transient ListState<Sale> recent;
	private transient Counter roundTrips;
	private transient Counter cycles;

	public WashTradeDetector(Duration window) {
		this.windowMs = window.toMillis();
	}

	@Override
	public void open(OpenContext ctx) {
		recent = getRuntimeContext().getListState(new ListStateDescriptor<>("recent-sales", Sale.class));
		roundTrips = getRuntimeContext().getMetricGroup().addGroup("rule", "round_trip").counter("analytics.wash_trades.flagged");
		cycles = getRuntimeContext().getMetricGroup().addGroup("rule", "cycle").counter("analytics.wash_trades.flagged");
	}

	@Override
	public void processElement(Sale sale, Context ctx, Collector<Flag> out) throws Exception {
		List<Sale> sales = new ArrayList<>();
		for (Sale s : recent.get()) {
			if (s.saleId.equals(sale.saleId)) {
				return; // A repeat of a sale already seen (the outbox publishes at least once).
			}
			sales.add(s);
		}

		Flag flag = loopClosedBy(sale, sales);
		if (flag != null) {
			(flag.roundTrip ? roundTrips : cycles).inc();
			out.collect(flag);
		}

		sales.add(sale);
		sales.sort(Comparator.comparingLong(s -> s.completedAtMs));
		if (sales.size() > MAX_SALES) {
			sales = new ArrayList<>(sales.subList(sales.size() - MAX_SALES, sales.size()));
		}
		recent.update(sales);
		ctx.timerService().registerEventTimeTimer(sale.completedAtMs + windowMs + 1);
	}

	/** The loop the sale closes, or null: sales are the drawing's earlier sales, oldest first. */
	Flag loopClosedBy(Sale sale, List<Sale> sales) {
		for (int j = sales.size() - 1; j >= 0; j--) {
			Sale earlier = sales.get(j);
			if (earlier.completedAtMs > sale.completedAtMs) {
				continue; // Not earlier after all; sales come in order, so this is a guard.
			}
			if (sale.completedAtMs - earlier.completedAtMs > windowMs) {
				return null; // Everything before this is older still.
			}
			if (earlier.sellerId.equals(sale.buyerId)) {
				return flag(sales.subList(j, sales.size()), sale);
			}
		}
		return null;
	}

	private static Flag flag(List<Sale> loop, Sale closing) {
		Flag f = new Flag();
		f.drawingId = closing.drawingId;
		f.roundTrip = loop.size() == 1;
		f.firstSaleAtMs = loop.get(0).completedAtMs;
		f.lastSaleAtMs = closing.completedAtMs;
		f.accountIds.add(loop.get(0).sellerId);
		for (Sale s : loop) {
			f.accountIds.add(s.buyerId);
			f.saleIds.add(s.saleId);
			f.volume += s.price;
		}
		f.accountIds.add(closing.buyerId);
		f.saleIds.add(closing.saleId);
		f.volume += closing.price;
		return f;
	}

	@Override
	public void onTimer(long timestamp, OnTimerContext ctx, Collector<Flag> out) throws Exception {
		List<Sale> kept = new ArrayList<>();
		for (Sale s : recent.get()) {
			if (s.completedAtMs + windowMs >= timestamp) {
				kept.add(s);
			}
		}
		if (kept.isEmpty()) {
			recent.clear();
		} else {
			recent.update(kept);
		}
	}
}
