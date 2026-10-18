package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

const cliOutputLimit = 32 << 20

// The command process owns output files; no path crosses the actor socket.
type cliOutputFile struct {
	file      *os.File
	size      int
	err       error
	keepEmpty bool
	root      *os.Root
	name      string
}

var _ io.Writer = (*cliOutputFile)(nil)

func (c *cliOutputFile) Write(data []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(data) > cliOutputLimit-c.size {
		c.err = errors.New("MCP response exceeds 32 MiB output file limit")
		return 0, c.err
	}
	n, err := c.file.Write(data)
	c.size += n
	c.err = err
	return n, err
}

func (c *cliOutputFile) finish(ctx context.Context) error {
	err := errors.Join(c.err, ctx.Err())
	complete := c.size > 0 || c.keepEmpty
	if err == nil && complete {
		err = c.file.Sync()
	}
	info, statErr := c.file.Stat()
	err = errors.Join(err, c.file.Close())
	// A synced descriptor is not a published artifact if its reserved path
	// disappeared or now names a different file.
	current, currentErr := c.statPath()
	switch {
	case statErr != nil:
		err = errors.Join(err, statErr, currentErr)
	case currentErr != nil:
		if complete || !errors.Is(currentErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("output path unavailable: %w", currentErr))
		}
	case !os.SameFile(info, current):
		err = errors.Join(err, errors.New("output path changed; replacement was not removed"))
	case err != nil || !complete:
		err = errors.Join(err, c.removePath())
	}
	if err != nil {
		return fmt.Errorf("failed to save MCP response (operation not replayed): %w", err)
	}
	return nil
}

func (c *cliOutputFile) statPath() (os.FileInfo, error) {
	if c.root != nil {
		return c.root.Lstat(c.name)
	}
	return os.Lstat(c.file.Name())
}

func (c *cliOutputFile) removePath() error {
	if c.root != nil {
		return c.root.Remove(c.name)
	}
	return os.Remove(c.file.Name())
}
