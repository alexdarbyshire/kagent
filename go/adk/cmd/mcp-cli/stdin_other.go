//go:build !linux

package main

import (
	"errors"
	"os"
)

func commandStdin() (*os.File, error) {
	return nil, errors.New("the standalone MCP CLI currently requires Linux")
}
