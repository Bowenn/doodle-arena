package com.doodlearena.marketplace.reward;

import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

import com.doodlearena.proto.doodle.events.v1.MatchFinished;
import com.doodlearena.proto.doodle.events.v1.PlayerResult;

/** A validated {@code MatchFinished}: the match and each player's placement. */
public record RewardRequest(String matchId, String roomId, List<Placement> placements) {

	public RewardRequest {
		placements = List.copyOf(placements);
	}

	public record Placement(String userId, int placement) {
	}

	/**
	 * Checks the event against its contract (proto/doodle/events/v1/matches.proto).
	 * @throws InvalidMatchEventException the event can never be paid, however often it is retried
	 */
	public static RewardRequest from(MatchFinished event) {
		if (event.getMatchId().isBlank()) {
			throw new InvalidMatchEventException("no match_id");
		}
		Set<String> seen = new HashSet<>();
		List<Placement> placements = new ArrayList<>();
		for (PlayerResult result : event.getResultsList()) {
			if (result.getUserId().isBlank() || !seen.add(result.getUserId())) {
				throw new InvalidMatchEventException("results has a blank or repeated user_id");
			}
			if (result.getPlacement() < 1) {
				throw new InvalidMatchEventException("placement starts at 1");
			}
			placements.add(new Placement(result.getUserId(), result.getPlacement()));
		}
		return new RewardRequest(event.getMatchId(), event.getRoomId(), placements);
	}

}
