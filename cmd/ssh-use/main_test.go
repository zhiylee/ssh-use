package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunUsage(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(nil, &stderr); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stderr.String(), "ssh-use exec") {
		t.Fatalf("usage missing: %q", stderr.String())
	}
}

func TestRunUnknown(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"wat"}, &stderr); code != 2 {
		t.Fatalf("code=%d", code)
	}
}

func TestRunDispatch(t *testing.T) {
	origExec := runExecFn
	origCopy := runCopyFn
	origTUI := runTUIFn
	origDaemon := runDaemonFn
	t.Cleanup(func() { runExecFn = origExec; runCopyFn = origCopy; runTUIFn = origTUI; runDaemonFn = origDaemon })

	runExecFn = func(args []string) int {
		if len(args) != 1 || args[0] != "x" {
			t.Fatalf("exec args=%#v", args)
		}
		return 11
	}
	if code := run([]string{"exec", "x"}, &bytes.Buffer{}); code != 11 {
		t.Fatalf("exec code=%d", code)
	}

	runCopyFn = func(args []string) int {
		if len(args) != 2 || args[0] != "a" || args[1] != "h:/b" {
			t.Fatalf("cp args=%#v", args)
		}
		return 13
	}
	if code := run([]string{"cp", "a", "h:/b"}, &bytes.Buffer{}); code != 13 {
		t.Fatalf("cp code=%d", code)
	}

	runTUIFn = func(args []string) int {
		if len(args) != 1 || args[0] != "--safe" {
			t.Fatalf("tui args=%#v", args)
		}
		return 12
	}
	if code := run([]string{"tui", "--safe"}, &bytes.Buffer{}); code != 12 {
		t.Fatalf("tui code=%d", code)
	}

	runDaemonFn = func(context.Context) error { return nil }
	if code := run([]string{"daemon"}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("daemon code=%d", code)
	}
	var stderr bytes.Buffer
	runDaemonFn = func(context.Context) error { return errors.New("bad daemon") }
	if code := run([]string{"daemon"}, &stderr); code != 1 || !strings.Contains(stderr.String(), "bad daemon") {
		t.Fatalf("daemon error code=%d stderr=%q", code, stderr.String())
	}
}
