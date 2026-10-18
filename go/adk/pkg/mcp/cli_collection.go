package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

const (
	cliCollectionLimit = 100
	cliManifestLimit   = 1 << 20
)

type cliResultCollection struct {
	fields []*cliResultField
	group  *cliResultField
}

func (c cliResultNames) selectCollection(name string) (cliResultCollection, error) {
	var selected cliResultCollection
	if c.literal[name] != nil {
		return selected, errors.New("named collection is a literal property, not an array field or native group")
	}
	matches := 0
	if fields := c.native[name]; len(fields) > 0 {
		selected.fields = fields
		matches++
	}
	for group, children := range c.groups {
		if children[name] != nil {
			matches++
		}
		if field, ok := strings.CutPrefix(name, group+"."); ok && children[field] != nil {
			matches++
		}
	}
	var member string
	for group, array := range c.arrays {
		if field, ok := strings.CutPrefix(name, group+"."); ok && field != "" {
			matches++
			selected.group, member = array, field
		}
	}
	if matches != 1 || (selected.group == nil && selected.fields == nil) {
		return selected, errors.New("named collection is missing or ambiguous; select an array field or native group")
	}
	if selected.group != nil {
		if selected.group.err != nil {
			return selected, selected.group.err
		}
		decoder := json.NewDecoder(bytes.NewReader(selected.group.value))
		if _, err := decoder.Token(); err != nil {
			return selected, err
		}
		for decoder.More() {
			if len(selected.fields) == cliCollectionLimit {
				return selected, errors.New("collection exceeds 100 files")
			}
			var row json.RawMessage
			if err := decoder.Decode(&row); err != nil {
				return selected, err
			}
			fields, err := cliResultObject(row)
			if err != nil {
				return selected, errors.New("collection requires object rows")
			}
			field := fields[member]
			if field == nil {
				return selected, errors.New("collection member missing; every row requires the selected string")
			}
			field.contentIndex = len(selected.fields)
			selected.fields = append(selected.fields, field)
		}
	}
	if len(selected.fields) > cliCollectionLimit {
		return selected, errors.New("collection exceeds 100 files")
	}
	return selected, nil
}

type cliCollectionManifest struct {
	Version      int                 `json:"version"`
	Target       string              `json:"target"`
	Conversion   string              `json:"conversion"`
	SourceSHA256 string              `json:"source_sha256"`
	Files        []cliCollectionFile `json:"files"`
}

type cliCollectionFile struct {
	Name        string `json:"name"`
	SourceIndex int    `json:"source_index"`
	Length      int    `json:"length"`
	SHA256      string `json:"sha256"`
}

// Root-relative operations keep writes and cleanup on the reserved directory
// even if its public path is renamed or replaced during the tool call.
type cliOutputDirectory struct {
	path     string
	root     *os.Root
	info     os.FileInfo
	children []cliOwnedCollectionFile
	complete bool
}

type cliOwnedCollectionFile struct {
	name string
	info os.FileInfo
	pin  *os.File
}

func createCLIOutputDirectory(path string) (*cliOutputDirectory, error) {
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, fmt.Errorf("failed to create MCP output directory (tool not called): %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect MCP output directory (tool not called): %w", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errors.Join(err, removeCLIOwnedPath(path, info))
	}
	directory := &cliOutputDirectory{path: path, root: root, info: info}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("output directory changed during reservation"), err, directory.finish())
	}
	return directory, nil
}

func removeCLIOwnedPath(path string, owned os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(owned, current) {
		return errors.New("output path changed; replacement was not removed")
	}
	return os.Remove(path)
}

func (c *cliOutputDirectory) checkPath() error {
	current, err := os.Lstat(c.path)
	if err != nil {
		return fmt.Errorf("output directory path unavailable: %w", err)
	}
	if !os.SameFile(c.info, current) {
		return errors.New("output directory path changed; replacement was not removed")
	}
	return nil
}

func (c *cliOutputDirectory) write(ctx context.Context, name string, data []byte) error {
	if err := errors.Join(ctx.Err(), c.checkPath()); err != nil {
		return err
	}
	file, err := c.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("failed to create collection file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return errors.Join(err, file.Close())
	}
	// Keep the inode alive after the writer closes. Otherwise an unlinked
	// finalized file's inode could be reused by a replacement before cleanup.
	pin, err := c.root.Open(name)
	child := cliOwnedCollectionFile{name: name, info: info, pin: pin}
	c.children = append(c.children, child)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	pinned, err := pin.Stat()
	if err != nil || !os.SameFile(info, pinned) {
		return errors.Join(errors.New("collection file changed during reservation"), err, file.Close())
	}
	output := &cliOutputFile{file: file, root: c.root, name: name}
	_, err = output.Write(data)
	output.keepEmpty = err == nil
	return errors.Join(err, output.finish(ctx))
}

func (c *cliOutputDirectory) save(ctx context.Context, envelope json.RawMessage, save cliSaveRequest, rawSHA256 string) error {
	names, err := projectCLIResult(envelope)
	if err != nil {
		return err
	}
	selected, err := names.selectCollection(save.name)
	if err != nil {
		return err
	}
	var digests cliResultCollection
	singularDigest := ""
	if save.digest != "" {
		if selected.group == nil {
			singularDigest, err = names.selectString(save.digest)
		} else {
			digests, err = names.selectCollection(save.digest)
			if err == nil && digests.group != selected.group {
				err = errors.New("collection SHA-256 must select a field from the same array group")
			}
		}
		if err != nil {
			return err
		}
	}
	conversion := "text"
	if save.base64 {
		conversion = "base64"
	}
	manifest := cliCollectionManifest{Version: 1, Target: save.name, Conversion: conversion, SourceSHA256: rawSHA256, Files: []cliCollectionFile{}}
	total := 0
	for i, field := range selected.fields {
		text, err := cliResultString(field)
		if err != nil {
			return err
		}
		digest := singularDigest
		if save.digest != "" && selected.group != nil {
			digest, err = cliResultString(digests.fields[i])
			if err != nil {
				return err
			}
		}
		data, err := convertCLIString(text, digest, save.base64, save.digest != "")
		if err != nil {
			return err
		}
		if len(data) > cliOutputLimit-total {
			return errors.New("collection exceeds 32 MiB aggregate payload limit")
		}
		total += len(data)
		name := fmt.Sprintf("%04d", i+1)
		if err := c.write(ctx, name, data); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, cliCollectionFile{Name: name, SourceIndex: field.contentIndex, Length: len(data), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > cliManifestLimit {
		return errors.New("collection manifest exceeds 1 MiB")
	}
	for _, child := range c.children {
		current, err := c.root.Lstat(child.name)
		if err != nil {
			return err
		}
		if !os.SameFile(child.info, current) {
			return errors.New("collection file changed; replacement was not removed")
		}
	}
	if err := c.write(ctx, "manifest.json", data); err != nil {
		return err
	}
	directory, err := c.root.Open(".")
	if err != nil {
		return err
	}
	if err := errors.Join(directory.Sync(), directory.Close(), ctx.Err(), c.checkPath()); err != nil {
		return err
	}
	c.complete = true
	return nil
}

func (c *cliOutputDirectory) finish() error {
	var err error
	if !c.complete {
		// Remove the completion marker before any payload, including when a
		// final directory sync or cancellation failed after writing it.
		for _, child := range slices.Backward(c.children) {
			current, statErr := c.root.Lstat(child.name)
			switch {
			case errors.Is(statErr, os.ErrNotExist):
			case statErr != nil:
				err = errors.Join(err, statErr)
			case !os.SameFile(child.info, current):
				err = errors.Join(err, errors.New("collection file changed; replacement was not removed"))
			default:
				err = errors.Join(err, c.root.Remove(child.name))
			}
		}
		err = errors.Join(err, removeCLIOwnedPath(c.path, c.info))
	}
	for _, child := range c.children {
		if child.pin != nil {
			err = errors.Join(err, child.pin.Close())
		}
	}
	err = errors.Join(err, c.root.Close())
	if err != nil {
		return fmt.Errorf("failed to finish MCP collection (operation not replayed): %w", err)
	}
	return nil
}
