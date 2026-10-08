package main

import (
	"fmt"
	"os"
)

// Reopening creates an independent file description and registers pipes with
// Go's poller, so Close interrupts a blocked read without changing parent flags.
func commandStdin() (*os.File, error) {
	file, err := os.Open("/proc/self/fd/0")
	if err != nil {
		return nil, fmt.Errorf("failed to open cancellable CLI stdin: %w", err)
	}
	return file, nil
}
