package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func runIn(t *testing.T, dir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"-dir", dir}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestSetThenGet(t *testing.T) {
	dir := t.TempDir()

	if code, _, stderr := runIn(t, dir, "set", "a", "1"); code != 0 {
		t.Fatalf("set exit = %d, stderr = %q", code, stderr)
	}
	code, stdout, stderr := runIn(t, dir, "get", "a")
	if code != 0 {
		t.Fatalf("get exit = %d, stderr = %q", code, stderr)
	}
	if got := strings.TrimSpace(stdout); got != "1" {
		t.Fatalf("get printed %q, want %q", got, "1")
	}
}

func TestGetMissingKeyFails(t *testing.T) {
	code, stdout, stderr := runIn(t, t.TempDir(), "get", "nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "key not found") {
		t.Fatalf("stderr = %q, want it to say the key was not found", stderr)
	}
}

func TestDeleteThenGet(t *testing.T) {
	dir := t.TempDir()
	if code, _, stderr := runIn(t, dir, "set", "k", "v"); code != 0 {
		t.Fatalf("set: exit %d, %s", code, stderr)
	}
	if code, _, stderr := runIn(t, dir, "del", "k"); code != 0 {
		t.Fatalf("del: exit %d, %s", code, stderr)
	}
	if code, _, _ := runIn(t, dir, "get", "k"); code != 1 {
		t.Fatalf("get after del exit = %d, want 1", code)
	}
}

func TestValueWithSpaces(t *testing.T) {
	dir := t.TempDir()
	if code, _, stderr := runIn(t, dir, "set", "greeting", "hello", "world"); code != 0 {
		t.Fatalf("set: exit %d, %s", code, stderr)
	}
	code, stdout, _ := runIn(t, dir, "get", "greeting")
	if code != 0 {
		t.Fatalf("get exit = %d", code)
	}
	if got := strings.TrimSpace(stdout); got != "hello world" {
		t.Fatalf("get = %q, want %q", got, "hello world")
	}
}

func TestCompactKeepsLiveKeys(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if code, _, stderr := runIn(t, dir, "set", "k", strings.Repeat("v", i+1)); code != 0 {
			t.Fatalf("set: exit %d, %s", code, stderr)
		}
	}
	if code, _, stderr := runIn(t, dir, "set", "dead", "v"); code != 0 {
		t.Fatalf("set dead: exit %d, %s", code, stderr)
	}
	if code, _, stderr := runIn(t, dir, "del", "dead"); code != 0 {
		t.Fatalf("del: exit %d, %s", code, stderr)
	}

	code, stdout, stderr := runIn(t, dir, "compact")
	if code != 0 {
		t.Fatalf("compact exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "compacted") {
		t.Fatalf("compact printed %q", stdout)
	}

	// Values must survive their index entries being rewritten.
	code, stdout, _ = runIn(t, dir, "get", "k")
	if code != 0 || strings.TrimSpace(stdout) != "vvvvv" {
		t.Fatalf("get after compact = %q, exit %d", stdout, code)
	}
	if code, _, _ := runIn(t, dir, "get", "dead"); code != 1 {
		t.Fatalf("deleted key survived compaction: exit %d", code)
	}
}

func TestUsageErrors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"explode"}},
		{"set without a value", []string{"set", "a"}},
		{"get without a key", []string{"get"}},
		{"del without a key", []string{"del"}},
		{"bad sync policy", []string{"-sync", "sometimes", "get", "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runIn(t, dir, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if stderr == "" {
				t.Fatal("nothing explained the problem on stderr")
			}
		})
	}
}

func TestDataDirectoryIsCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "store")
	if code, _, stderr := runIn(t, dir, "set", "a", "1"); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
}
