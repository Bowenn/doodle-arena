package com.doodlearena.marketplace.ownership;

import java.util.HashSet;
import java.util.List;
import java.util.UUID;

import com.doodlearena.proto.doodle.events.v1.DrawingApproved;

/**
 * A validated {@code DrawingApproved}: what minting needs, nothing else.
 * @param sourceEventId the event's {@code meta.event_id}, kept on the MINT row for provenance
 */
public record MintRequest(UUID drawingId, String matchId, int roundNo, List<String> authorIds, String initialOwnerId,
		String sourceEventId) {

	public MintRequest {
		authorIds = List.copyOf(authorIds);
	}

	/**
	 * Checks the event against its contract (proto/doodle/events/v1/drawings.proto).
	 * @throws InvalidDrawingEventException the event can never be minted, however often it is retried
	 */
	public static MintRequest from(DrawingApproved event) {
		UUID drawingId;
		try {
			drawingId = UUID.fromString(event.getDrawingId());
		}
		catch (IllegalArgumentException ex) {
			throw new InvalidDrawingEventException("drawing_id is not a UUID");
		}
		List<String> authors = event.getAuthorIdsList();
		if (authors.isEmpty()) {
			throw new InvalidDrawingEventException("no author_ids");
		}
		if (authors.stream().anyMatch(String::isBlank) || new HashSet<>(authors).size() != authors.size()) {
			throw new InvalidDrawingEventException("author_ids has a blank or repeated id");
		}
		if (!event.getInitialOwnerId().equals(authors.getFirst())) {
			throw new InvalidDrawingEventException("initial_owner_id is not the first author");
		}
		return new MintRequest(drawingId, event.getMatchId(), event.getRoundNo(), authors, event.getInitialOwnerId(),
				event.getMeta().getEventId());
	}

}
