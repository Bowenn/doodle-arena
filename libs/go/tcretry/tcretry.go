// Package tcretry starts Testcontainers containers with a few retries. go test runs packages
// in parallel, and packages that start their first container at the same moment can race on
// Testcontainers' shared reaper ("wait for reaper ...: unexpected container status created"):
// a failure that says nothing about the code under test, found by the task 5.5 flake pass.
package tcretry

import (
	"fmt"
	"os"
	"time"
)

// Attempts is how many times Run tries.
const Attempts = 3

// Run calls start until it succeeds, at most Attempts times, waiting 2 s, then 4 s, between
// tries, and returns the last error. what names the container in the log.
func Run[T any](what string, start func() (T, error)) (T, error) {
	var (
		c   T
		err error
	)
	wait := 2 * time.Second
	for attempt := 1; attempt <= Attempts; attempt++ {
		if c, err = start(); err == nil {
			return c, nil
		}
		if attempt < Attempts {
			fmt.Fprintf(os.Stderr, "start %s (attempt %d of %d): %v; retrying in %s\n", what, attempt, Attempts, err, wait)
			time.Sleep(wait)
			wait *= 2
		}
	}
	return c, err
}
