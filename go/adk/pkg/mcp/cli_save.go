package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type cliSaveRequest struct {
	name, path, digest string
	base64             bool
	collection         bool
}

type cliLocalOutputs struct {
	raw, artifact *cliOutputFile
	directory     *cliOutputDirectory
}

func reserveCLIOutputs(ctx context.Context, command cliLocalCommand) (_ *cliLocalOutputs, err error) {
	outputs := &cliLocalOutputs{}
	defer func() {
		if err != nil {
			err = errors.Join(err, outputs.finish(ctx))
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if command.outputPath != "" {
		outputs.raw, err = createCLIOutput(command.outputPath)
		if err != nil {
			return nil, err
		}
	}
	if command.save != nil {
		if command.save.collection {
			outputs.directory, err = createCLIOutputDirectory(command.save.path)
		} else {
			outputs.artifact, err = createCLIOutput(command.save.path)
		}
		if err != nil {
			return nil, err
		}
	}
	return outputs, nil
}

func createCLIOutput(path string) (*cliOutputFile, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create MCP output file (tool not called): %w", err)
	}
	return &cliOutputFile{file: file}, nil
}

func (c *cliLocalOutputs) finish(ctx context.Context) error {
	var err error
	if c.raw != nil {
		err = c.raw.finish(ctx)
		c.raw = nil
	}
	if c.artifact != nil {
		err = errors.Join(err, c.artifact.finish(ctx))
		c.artifact = nil
	}
	if c.directory != nil {
		err = errors.Join(err, c.directory.finish())
		c.directory = nil
	}
	return err
}

func (c *cliLocalOutputs) handle(ctx context.Context, stdout io.Writer, outcome cliOutcome, save *cliSaveRequest) error {
	if c.raw == nil {
		return renderCLIOutcome(stdout, outcome)
	}
	result, ok := outcome.(cliResult)
	if !ok {
		return renderCLIOutcome(c.raw, outcome)
	}
	rawHash := sha256.New()
	writeErr := json.NewEncoder(io.MultiWriter(c.raw, rawHash)).Encode(result.Envelope)
	if writeErr != nil {
		writeErr = fmt.Errorf("failed to write MCP result (operation not replayed): %w", writeErr)
	}
	rawErr := errors.Join(writeErr, c.raw.finish(ctx))
	c.raw = nil
	if rawErr != nil || save == nil {
		return errors.Join(result.Err, rawErr)
	}
	return c.materialize(ctx, result, *save, hex.EncodeToString(rawHash.Sum(nil)))
}

func (c *cliLocalOutputs) materialize(ctx context.Context, result cliResult, save cliSaveRequest, sourceHash string) error {
	if save.collection {
		saveErr := c.directory.save(ctx, result.Envelope, save, sourceHash)
		if saveErr != nil {
			saveErr = fmt.Errorf("failed to save MCP collection (operation not replayed): %w", saveErr)
		}
		return errors.Join(result.Err, saveErr)
	}
	data, saveErr := materializeCLIString(result.Envelope, save)
	if saveErr != nil {
		return errors.Join(result.Err, fmt.Errorf("failed to save named MCP output (operation not replayed): %w", saveErr))
	}
	_, writeErr := c.artifact.Write(data)
	c.artifact.keepEmpty = writeErr == nil
	finishErr := c.artifact.finish(ctx)
	c.artifact = nil
	return errors.Join(result.Err, writeErr, finishErr)
}

func materializeCLIString(envelope json.RawMessage, save cliSaveRequest) ([]byte, error) {
	names, err := projectCLIResult(envelope)
	if err != nil {
		return nil, err
	}
	text, err := names.selectString(save.name)
	if err != nil {
		return nil, err
	}
	digest := ""
	if save.digest != "" {
		digest, err = names.selectString(save.digest)
		if err != nil {
			return nil, err
		}
	}
	return convertCLIString(text, digest, save.base64, save.digest != "")
}

func convertCLIString(text, digest string, decodeBase64, verify bool) ([]byte, error) {
	if len(text) > cliOutputLimit {
		return nil, errors.New("selected output string exceeds 32 MiB")
	}
	data := []byte(text)
	if decodeBase64 {
		decoded, err := base64.StdEncoding.Strict().DecodeString(text)
		if err != nil || strings.ContainsAny(text, "\r\n") {
			return nil, errors.New("named output requires canonical standard padded base64")
		}
		if len(decoded) > 24<<20 {
			return nil, errors.New("decoded output exceeds 24 MiB")
		}
		data = decoded
	}
	if verify {
		expected, err := hex.DecodeString(digest)
		if err != nil || len(digest) != 64 {
			return nil, errors.New("selected SHA-256 must contain 64 hex digits")
		}
		actual := sha256.Sum256(data)
		if !bytes.Equal(expected, actual[:]) {
			return nil, errors.New("selected SHA-256 mismatch")
		}
	}
	return data, nil
}
