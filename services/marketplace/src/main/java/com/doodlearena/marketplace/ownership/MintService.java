package com.doodlearena.marketplace.ownership;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

/**
 * Mints approved drawings (design doc, section 7.6): the first owner, the authors in drawing
 * order and a MINT event, in one transaction. Kafka delivers at least once, so minting is
 * idempotent by drawing id: a redelivery finds the ownership row and writes nothing.
 */
@Service
public class MintService {

	private static final Logger log = LoggerFactory.getLogger(MintService.class);

	private final JdbcClient db;

	private final Tracer tracer = GlobalOpenTelemetry.getTracer("marketplace");

	public MintService(JdbcClient db) {
		this.db = db;
	}

	public enum MintResult {

		MINTED, DUPLICATE

	}

	@Transactional
	@SuppressWarnings("try") // the scope only has to be open while the statements run
	public MintResult mint(MintRequest request) {
		Span span = this.tracer.spanBuilder("market.mint")
			.setAttribute("drawing.id", request.drawingId().toString())
			.setAttribute("match.id", request.matchId())
			.startSpan();
		try (Scope scope = span.makeCurrent()) {
			// A concurrent mint of the same drawing waits on the primary key, then finds the row.
			boolean created = this.db.sql("""
					INSERT INTO ownership (drawing_id, owner_id) VALUES (?, ?)
					ON CONFLICT (drawing_id) DO NOTHING
					RETURNING drawing_id""")
				.params(request.drawingId(), request.initialOwnerId())
				.query()
				.listOfRows()
				.size() == 1;
			span.setAttribute("mint.duplicate", !created);
			if (!created) {
				log.atInfo()
					.addKeyValue("drawing_id", request.drawingId())
					.addKeyValue("match_id", request.matchId())
					.log("drawing already minted");
				return MintResult.DUPLICATE;
			}
			for (int i = 0; i < request.authorIds().size(); i++) {
				this.db.sql("INSERT INTO drawing_authors (drawing_id, user_id, author_order) VALUES (?, ?, ?)")
					.params(request.drawingId(), request.authorIds().get(i), i)
					.update();
			}
			this.db.sql("""
					INSERT INTO ownership_events (drawing_id, kind, to_owner, source_event_id)
					VALUES (?, 'MINT', ?, ?)""")
				.params(request.drawingId(), request.initialOwnerId(), request.sourceEventId())
				.update();
			log.atInfo()
				.addKeyValue("drawing_id", request.drawingId())
				.addKeyValue("match_id", request.matchId())
				.addKeyValue("user_id", request.initialOwnerId())
				.addKeyValue("authors", request.authorIds().size())
				.log("drawing minted");
			return MintResult.MINTED;
		}
		catch (RuntimeException ex) {
			span.recordException(ex);
			span.setStatus(StatusCode.ERROR);
			throw ex;
		}
		finally {
			span.end();
		}
	}

}
