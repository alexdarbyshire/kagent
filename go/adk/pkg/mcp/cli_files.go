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

// File values are strings even when a parameter also accepts other JSON types.
// They cross the private socket separately from argv and never carry host paths.
type cliFileValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cliOption struct {
	name  string
	value string
	file  bool
}

func cliOptions(args []string) ([]cliOption, error) {
	var options []cliOption
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") {
			return nil, fmt.Errorf("expected argument flag, got %q", args[i])
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if name == "help" {
			return nil, errors.New("--help must be used alone; use JSON input for the help property")
		}
		if !inline {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("missing value for --%s", name)
			}
			value = args[i]
		}
		options = append(options, cliOption{name: name, value: value})
	}
	return options, nil
}

func cliReservedFlag(name string) bool {
	return name == "help" || name == "input-file" || name == "base64-file" || name == "text-file"
}

func prepareCLIFileValues(ctx context.Context, args []string) ([]string, []cliFileValue, error) {
	if len(args) > 1 && args[0] == "--help" {
		return nil, nil, errors.New("--help must be used alone")
	}
	if len(args) < 2 || (args[1] == "--help" && (len(args) == 2 || (len(args) == 3 && args[2] == "schema"))) {
		return args, nil, nil
	}
	options, err := cliOptions(args[1:])
	if err != nil {
		return nil, nil, err
	}
	for _, option := range options {
		if option.name == "input-file" && len(options) != 1 {
			return nil, nil, errors.New("--input-file cannot be combined with argument flags")
		}
	}
	result := []string{args[0]}
	var files []cliFileValue
	total := 0
	for _, option := range options {
		if option.name != "base64-file" && option.name != "text-file" {
			result = append(result, "--"+option.name+"="+option.value)
			continue
		}
		name, path, ok := strings.Cut(option.value, "=")
		if !ok || !cliFlagName(name) || strings.HasPrefix(name, "-") || path == "" || path == "-" {
			return nil, nil, fmt.Errorf("--%s requires NAME=PATH with an exposed parameter name and a regular file path", option.name)
		}
		data, err := readCLIValueFile(ctx, path, min(cliFileLimit, cliFilesLimit-total))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read --%s target %s from %q: %w", option.name, name, path, err)
		}
		total += len(data)
		value := ""
		if option.name == "base64-file" {
			value = base64.StdEncoding.EncodeToString(data)
		} else {
			if !utf8.Valid(data) {
				return nil, nil, fmt.Errorf("--text-file target %s from %q must contain UTF-8 text", name, path)
			}
			value = string(data)
		}
		files = append(files, cliFileValue{Name: name, Value: value})
	}
	return result, files, nil
}

func readCLIValueFile(ctx context.Context, path string, limit int) (data []byte, err error) {
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
		return nil, errors.New("file inputs exceed the 8 MiB per-file or 16 MiB aggregate limit")
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
		return nil, errors.New("file inputs exceed the 8 MiB per-file or 16 MiB aggregate limit")
	}
	return data, ctx.Err()
}

func cliFileValidationError(err error) error {
	var validation *schemaValidator.ValidationError
	if !errors.As(err, &validation) {
		return errors.New("validation failed for file-backed arguments; use --help schema for constraints")
	}
	for len(validation.Causes) > 0 {
		validation = validation.Causes[0]
	}
	// Formatted validator causes may include input or enum/const values. Report
	// only the first failing location and keyword, without retaining those causes.
	constraint := strings.Join(validation.ErrorKind.KeywordPath(), "/")
	if constraint == "" {
		constraint = validation.SchemaURL
	}
	parts := make([]string, len(validation.InstanceLocation))
	for i, part := range validation.InstanceLocation {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~", "~0"), "/", "~1")
	}
	return fmt.Errorf("validation failed at /%s (%s); use --help schema for constraints", strings.Join(parts, "/"), constraint)
}
