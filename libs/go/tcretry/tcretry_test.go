package tcretry

import (
	"errors"
	"testing"
)

func TestRunRetriesUntilStartSucceeds(t *testing.T) {
	calls := 0
	got, err := Run("thing", func() (int, error) {
		calls++
		if calls < 2 {
			return 0, errors.New("reaper not ready")
		}
		return 42, nil
	})
	if err != nil || got != 42 || calls != 2 {
		t.Fatalf("Run = %d, %v after %d calls; want 42 after 2", got, err, calls)
	}
}

func TestRunReturnsTheLastErrorAfterAllAttempts(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 6 s")
	}
	calls := 0
	_, err := Run("thing", func() (int, error) {
		calls++
		return 0, errors.New("down")
	})
	if err == nil || calls != Attempts {
		t.Fatalf("err = %v after %d calls; want an error after %d", err, calls, Attempts)
	}
}
