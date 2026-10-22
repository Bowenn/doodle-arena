package com.doodlearena.analytics.guesses;

import java.time.Duration;
import java.time.LocalDateTime;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import org.apache.flink.api.common.functions.OpenContext;
import org.apache.flink.api.common.functions.RichMapFunction;
import org.apache.flink.metrics.Counter;
import org.apache.flink.streaming.api.datastream.DataStream;
import org.apache.flink.streaming.api.functions.ProcessFunction;
import org.apache.flink.table.api.DataTypes;
import org.apache.flink.table.api.Schema;
import org.apache.flink.table.api.Table;
import org.apache.flink.table.api.bridge.java.StreamTableEnvironment;
import org.apache.flink.types.Row;
import org.apache.flink.util.Collector;

/**
 * The top-guesses job (ADR 0018, design doc 4.9): each live room's most common wrong guesses over
 * a hopping window, in Flink SQL.
 *
 * <p>The guesses arrive with event time (the relay's guessed_at) and watermarks. A window TVF
 * (HOP) counts each guess per room and round, a window Top-N (ROW_NUMBER over the window) keeps
 * the most common, and a second window aggregation over the same windows folds a window's top
 * guesses into one row, which becomes one WindowTop (one TopGuesses at the sink). Window results are final once the
 * watermark passes the window's end, so the stream stays append-only.
 */
public final class TopGuessesQuery {
	private TopGuessesQuery() {}

	/** Builds the query over guesses (with timestamps and watermarks already assigned). */
	public static DataStream<WindowTop> apply(StreamTableEnvironment tEnv, DataStream<Guess> guesses, Duration window,
			Duration slide, int topN) {
		// Windows are reported as UTC: window_end is a TIMESTAMP, read below as UTC.
		tEnv.getConfig().setLocalTimeZone(ZoneOffset.UTC);
		DataStream<Guess> counted = guesses.process(new CountLate()).name("count late guesses");
		Table table = tEnv.fromDataStream(counted, Schema.newBuilder()
				.column("roomId", DataTypes.STRING())
				.column("roundNo", DataTypes.INT())
				.column("text", DataTypes.STRING())
				.column("guessedAtMs", DataTypes.BIGINT())
				.columnByMetadata("rowtime", DataTypes.TIMESTAMP_LTZ(3))
				.watermark("rowtime", "SOURCE_WATERMARK()")
				.build());
		tEnv.createTemporaryView("guesses", table);
		Table top = tEnv.sqlQuery(String.format("""
				SELECT room_id, round_no, window_end,
				       LISTAGG(text || ':' || CAST(cnt AS STRING), ',') AS ranked
				FROM (
				  SELECT window_start, window_end, room_id, round_no, text, cnt
				  FROM (
				    SELECT *, ROW_NUMBER() OVER (
				             PARTITION BY window_start, window_end, room_id, round_no
				             ORDER BY cnt DESC, text ASC) AS rn
				    FROM (
				      SELECT window_start, window_end, roomId AS room_id, roundNo AS round_no, text,
				             COUNT(*) AS cnt
				      FROM TABLE(HOP(TABLE guesses, DESCRIPTOR(rowtime),
				                     INTERVAL '%d' SECOND, INTERVAL '%d' SECOND))
				      GROUP BY window_start, window_end, roomId, roundNo, text))
				  WHERE rn <= %d)
				GROUP BY window_start, window_end, room_id, round_no
				""", slide.toSeconds(), window.toSeconds(), topN));
		return tEnv.toDataStream(top).map(new ToWindowTop(topN)).name("rank each window");
	}

	/**
	 * Counts guesses that arrive behind the watermark: the window they belong to has already been
	 * reported, so the query drops them (analytics.guesses.late). They still pass through.
	 */
	static final class CountLate extends ProcessFunction<Guess, Guess> {
		private transient Counter late;

		@Override
		public void open(OpenContext ctx) {
			late = getRuntimeContext().getMetricGroup().counter("analytics.guesses.late");
		}

		@Override
		public void processElement(Guess g, Context ctx, Collector<Guess> out) {
			Long ts = ctx.timestamp();
			if (ts != null && ts <= ctx.timerService().currentWatermark()) {
				late.inc();
			}
			out.collect(g);
		}
	}

	/** One window's row to a WindowTop: the guesses ranked again (LISTAGG keeps no order). */
	static final class ToWindowTop extends RichMapFunction<Row, WindowTop> {
		private final int topN;
		private transient Counter emitted;

		ToWindowTop(int topN) {
			this.topN = topN;
		}

		@Override
		public void open(OpenContext ctx) {
			emitted = getRuntimeContext().getMetricGroup().counter("analytics.top_guesses.emitted");
		}

		@Override
		public WindowTop map(Row row) {
			List<String[]> pairs = new ArrayList<>();
			for (String pair : ((String) row.getField("ranked")).split(",")) {
				int colon = pair.lastIndexOf(':');
				pairs.add(new String[] {pair.substring(0, colon), pair.substring(colon + 1)});
			}
			// Most common first; ties by text, so a replay ranks the same way.
			pairs.sort(Comparator.<String[]>comparingInt(p -> -Integer.parseInt(p[1])).thenComparing(p -> p[0]));
			WindowTop w = new WindowTop();
			w.roomId = (String) row.getField("room_id");
			w.roundNo = (Integer) row.getField("round_no");
			w.windowEndMs = ((LocalDateTime) row.getField("window_end")).toInstant(ZoneOffset.UTC).toEpochMilli();
			for (String[] p : pairs.subList(0, Math.min(topN, pairs.size()))) {
				w.texts.add(p[0]);
				w.counts.add(Integer.parseInt(p[1]));
			}
			if (emitted != null) {
				emitted.inc();
			}
			return w;
		}
	}
}
