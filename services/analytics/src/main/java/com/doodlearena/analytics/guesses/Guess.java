package com.doodlearena.analytics.guesses;

/**
 * A viewer's wrong guess, as the top-guesses query reads it. A Flink POJO (public fields, a no-arg
 * constructor), so it becomes a table without a custom type.
 */
public class Guess {
	public String roomId;
	public int roundNo;
	public String text;
	/** When the relay received it (event time), in epoch milliseconds. */
	public long guessedAtMs;

	public Guess() {}

	public Guess(String roomId, int roundNo, String text, long guessedAtMs) {
		this.roomId = roomId;
		this.roundNo = roundNo;
		this.text = text;
		this.guessedAtMs = guessedAtMs;
	}
}
