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
	file *os.File
	size int
	err  error
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
	if err == nil && c.size > 0 {
		err = c.file.Sync()
	}
	info, statErr := c.file.Stat()
	err = errors.Join(err, c.file.Close())
	if err != nil || c.size == 0 {
		current, currentErr := os.Lstat(c.file.Name())
		switch {
		case errors.Is(currentErr, os.ErrNotExist):
		case statErr != nil || currentErr != nil:
			err = errors.Join(err, statErr, currentErr)
		case !os.SameFile(info, current):
			err = errors.Join(err, errors.New("output path changed; replacement was not removed"))
		default:
			err = errors.Join(err, os.Remove(c.file.Name()))
		}
	}
	if err != nil {
		return fmt.Errorf("failed to save MCP response (operation not replayed): %w", err)
	}
	return nil
}
