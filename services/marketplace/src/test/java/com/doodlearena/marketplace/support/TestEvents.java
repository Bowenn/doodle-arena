package com.doodlearena.marketplace.support;

import java.util.List;
import java.util.UUID;

import com.doodlearena.proto.doodle.events.v1.DrawingApproved;
import com.doodlearena.proto.doodle.events.v1.EventMeta;

/** Events shaped like their producers' (proto/doodle/events/v1). */
public final class TestEvents {

	private TestEvents() {
	}

	/** A DrawingApproved as assets publishes it: the first author is the initial owner. */
	public static DrawingApproved approved(UUID drawing, String... authors) {
		return DrawingApproved.newBuilder()
			.setMeta(EventMeta.newBuilder().setEventId("evt-" + drawing).setEventType("DrawingApproved").setSchemaVersion(1))
			.setDrawingId(drawing.toString())
			.setMatchId("m_test")
			.setRoundNo(1)
			.addAllAuthorIds(List.of(authors))
			.setInitialOwnerId(authors.length > 0 ? authors[0] : "")
			.build();
	}

}
