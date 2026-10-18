package mcp

import "fmt"

type cliUsageError struct{ cause error }

var _ error = (*cliUsageError)(nil)

func (c *cliUsageError) Error() string { return c.cause.Error() }
func (c *cliUsageError) Unwrap() error { return c.cause }

func cliUsageErrorf(format string, args ...any) error {
	return &cliUsageError{cause: fmt.Errorf(format, args...)}
}

// CLIExitCode returns 0 for success, 2 for invalid usage, and 1 for operational
// failure. A joined operational failure takes precedence over invalid usage.
func CLIExitCode(err error) int {
	if err == nil {
		return 0
	}
	if _, ok := err.(*cliUsageError); ok {
		return 2
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		code := 0
		for _, cause := range wrapped.Unwrap() {
			status := CLIExitCode(cause)
			if status == 1 {
				return 1
			}
			if status == 2 {
				code = 2
			}
		}
		if code != 0 {
			return code
		}
	case interface{ Unwrap() error }:
		if cause := wrapped.Unwrap(); cause != nil {
			return CLIExitCode(cause)
		}
	}
	return 1
}
