package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCommandOutputBound(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("\u754c", 430000)
	if err := os.WriteFile(filepath.Join(dir, "body"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		// The unquoted ampersand detaches retrieval before its intended output flag.
		"curl() { cat body; }; curl https://fixture.invalid/?x=1&y=2 -o saved; wait",
		"cat body",
		"cat body >&2; exit 7",
		"cat body; cat body >&2",
	} {
		output, err := NewCommandExecutor(ExecutionConfig{}).ExecuteCommand(context.Background(), command, dir)
		if err != nil {
			output = err.Error()
		}
		if len(output) > 17000 {
			t.Errorf("unbounded output: %d bytes", len(output))
		}
		if !strings.Contains(output, "output truncated") {
			t.Error("missing truncation notice")
		}
		if !utf8.ValidString(output) {
			t.Error("invalid UTF-8")
		}
	}
}

func TestCommandSmallOutputUnchanged(t *testing.T) {
	for _, command := range []string{"printf 'hello\\n'", "printf 'hello'; printf 'detail' >&2", "true"} {
		output, err := NewCommandExecutor(ExecutionConfig{}).ExecuteCommand(context.Background(), command, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		expected := map[string]string{"printf 'hello\\n'": "hello", "printf 'hello'; printf 'detail' >&2": "hello\ndetail", "true": "Command completed successfully."}[command]
		if output != expected {
			t.Errorf("got %q, want %q", output, expected)
		}
	}
}

func TestFailedCommandPreservesBoundedResultAndDiagnostic(t *testing.T) {
	executor := NewCommandExecutor(ExecutionConfig{})
	_, err := executor.ExecuteCommand(t.Context(), "printf '%s' '{\"isError\":true,\"artifact\":\"screenshot-fixture.png\"}'; printf '%s' 'provider denied' >&2; exit 1", t.TempDir())
	if err == nil {
		t.Fatal("failed command succeeded")
	}
	if !strings.Contains(err.Error(), "screenshot-fixture.png") || !strings.Contains(err.Error(), "provider denied") {
		t.Fatalf("failed command discarded result or diagnostic: %v", err)
	}
}
