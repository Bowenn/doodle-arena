package com.doodlearena.marketplace.tip;

import java.util.HashSet;
import java.util.List;

import com.doodlearena.proto.doodle.events.v1.RoundFinished;

/** A validated {@code RoundFinished}: who drew the round, so its tips can be paid (ADR 0021). */
public record RoundRequest(String matchId, String roomId, int roundNo, List<String> drawerIds) {

	/** One drawer, or two sharing a canvas (design doc, section 4.4). */
	public static final int MAX_DRAWERS = 2;

	public RoundRequest {
		drawerIds = List.copyOf(drawerIds);
	}

	/**
	 * Checks the event against its contract (proto/doodle/events/v1/rounds.proto).
	 * @throws InvalidRoundEventException the event can never be settled, however often it is retried
	 */
	public static RoundRequest from(RoundFinished event) {
		if (event.getMatchId().isBlank() || event.getRoomId().isBlank()) {
			throw new InvalidRoundEventException("no match_id or room_id");
		}
		// uint32: Java reads one above Integer.MAX_VALUE as negative.
		if (event.getRoundNo() < 1) {
			throw new InvalidRoundEventException("round_no starts at 1");
		}
		List<String> drawers = event.getDrawerIdsList();
		if (drawers.isEmpty() || drawers.size() > MAX_DRAWERS) {
			throw new InvalidRoundEventException("a round has one or two drawers");
		}
		if (drawers.stream().anyMatch(String::isBlank) || new HashSet<>(drawers).size() != drawers.size()) {
			throw new InvalidRoundEventException("drawer_ids has a blank or repeated user id");
		}
		return new RoundRequest(event.getMatchId(), event.getRoomId(), event.getRoundNo(), drawers);
	}

}
