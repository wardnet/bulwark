// Package executil runs external scanner tools and captures their output
// uniformly, so every language package reports results the same way.
package executil

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
)

// Result is the outcome of running one external command.
type Result struct {
	Name   string
	Args   []string
	Output string
	Err    error
}

// Ok reports whether the command exited zero.
func (r Result) Ok() bool { return r.Err == nil }

// Run executes name with args in dir, streaming combined stdout+stderr live
// to the terminal while also capturing it into the returned Result.
//
// name and args are always static, hardcoded tool invocations from bulwark's
// own scanner packages (cargo, npx, gosec, go, semgrep, pipx) — never built
// from user input or shell-interpreted, so there is no injection surface here.
func Run(ctx context.Context, dir, name string, args ...string) Result {
	return run(ctx, dir, nil, name, args...)
}

// RunEnv is Run with extra "KEY=value" entries appended to the child's
// environment (e.g. GOBIN, to control where `go install` places a binary).
func RunEnv(ctx context.Context, dir string, extraEnv []string, name string, args ...string) Result {
	return run(ctx, dir, extraEnv, name, args...)
}

func run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) Result {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command -- name/args are static, hardcoded tool invocations, never user input or shell-interpreted
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	// Stdout and Stderr being distinct writers makes os/exec copy each stream
	// on its own goroutine, and both feed the same capture buffer — so the
	// buffer writes must be locked.
	var buf bytes.Buffer
	captured := &lockedWriter{w: &buf}
	cmd.Stdout = io.MultiWriter(os.Stdout, captured)
	cmd.Stderr = io.MultiWriter(os.Stderr, captured)
	err := cmd.Run()
	return Result{Name: name, Args: args, Output: buf.String(), Err: err}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Available reports whether name is resolvable on PATH.
func Available(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
