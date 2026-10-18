package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	schemaValidator "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	cliFileLimit    = 8 << 20
	cliFilesLimit   = 16 << 20
	cliRequestLimit = 24 << 20
)

func prepareCLIFileValues(ctx context.Context, inputs []cliLocalFile) ([]cliAssignment, error) {
	var files []cliAssignment
	total := 0
	for _, input := range inputs {
		data, err := readCLIValueFile(ctx, input.path, min(cliFileLimit, cliFilesLimit-total))
		if err != nil {
			return nil, fmt.Errorf("failed to read --%s target %s from %q: %w", input.mode, input.name, input.path, err)
		}
		total += len(data)
		value := ""
		if input.mode == "base64-file" {
			value = base64.StdEncoding.EncodeToString(data)
		} else {
			if !utf8.Valid(data) {
				return nil, cliUsageErrorf("--text-file target %s from %q must contain UTF-8 text", input.name, input.path)
			}
			value = string(data)
		}
		files = append(files, cliAssignment{Name: input.name, Value: value, Literal: true})
	}
	return files, nil
}

func readCLIValueFile(ctx context.Context, path string, limit int) ([]byte, error) {
	return readCLIBoundedFile(ctx, path, limit, cliUsageErrorf("file inputs exceed the 8 MiB per-file or 16 MiB aggregate limit"))
}

func readCLIBoundedFile(ctx context.Context, path string, limit int, limitErr error) (data []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Nonblocking open prevents a replaced path from blocking on a FIFO before
	// fstat can verify the opened descriptor. The CLI supports Linux only.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); !errors.Is(closeErr, os.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("input must be a regular file")
	}
	if info.Size() > int64(limit) {
		return nil, limitErr
	}
	reader := struct {
		io.Reader
		io.Closer
	}{io.LimitReader(file, int64(limit)+1), file}
	data, err = readCLIInput(ctx, reader)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, limitErr
	}
	return data, ctx.Err()
}

func cliFileValidationError(err error) error {
	var validation *schemaValidator.ValidationError
	if !errors.As(err, &validation) {
		return &cliFileArgumentError{cause: err, diagnostic: "validation failed for file-backed arguments; use --help schema for constraints"}
	}
	for len(validation.Causes) > 0 {
		validation = validation.Causes[0]
	}
	// Formatted validator causes may include input or enum/const values. Report
	// only the first failing location and keyword; keep the cause private.
	constraint := strings.Join(validation.ErrorKind.KeywordPath(), "/")
	if constraint == "" {
		constraint = validation.SchemaURL
	}
	parts := make([]string, len(validation.InstanceLocation))
	for i, part := range validation.InstanceLocation {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~", "~0"), "/", "~1")
	}
	return &cliFileArgumentError{cause: err, diagnostic: fmt.Sprintf("validation failed at /%s (%s); use --help schema for constraints", strings.Join(parts, "/"), constraint)}
}

type cliFileArgumentError struct {
	cause      error
	diagnostic string
}

var _ error = (*cliFileArgumentError)(nil)

func (c *cliFileArgumentError) Error() string { return c.diagnostic }
func (c *cliFileArgumentError) Unwrap() error { return c.cause }
