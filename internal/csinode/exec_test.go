// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package csinode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// fakeCommandRunner is the CommandRunner double every other test in this
// package uses to exercise iSCSI login/logout/format orchestration
// without a real iscsiadm/blkid/mkfs binary. Records every call it sees
// (for assertions on exact command sequencing) and returns a
// caller-configured response per command name, falling back to success
// with empty output for anything not explicitly configured.
type fakeCommandRunner struct {
	mu       sync.Mutex
	calls    [][]string
	byName   map[string]func(args ...string) (string, error)
	fallback func(name string, args ...string) (string, error)
}

func newFakeCommandRunner() *fakeCommandRunner {
	return &fakeCommandRunner{byName: map[string]func(args ...string) (string, error){}}
}

func (f *fakeCommandRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	handler := f.byName[name]
	fallback := f.fallback
	f.mu.Unlock()
	if handler != nil {
		return handler(args...)
	}
	if fallback != nil {
		return fallback(name, args...)
	}
	return "", nil
}

func (f *fakeCommandRunner) on(name string, handler func(args ...string) (string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byName[name] = handler
}

func (f *fakeCommandRunner) callsFor(name string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == name {
			out = append(out, c)
		}
	}
	return out
}

// fakeExitError builds an *exec.ExitError reporting the given exit code
// -- used to simulate blkid's documented exit-2 "no filesystem found"
// signal without actually running blkid. Real *exec.ExitError values can
// only come from a real exec.Cmd.Run(), so this shells out to the
// test binary's own environment via `sh -c "exit N"`, the standard Go
// trick for constructing one deterministically.
func fakeExitError(code int) error {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
	err := cmd.Run()
	if err == nil {
		panic("fakeExitError: exit 0 did not produce an error")
	}
	return err
}

func joinArgs(args []string) string { return strings.Join(args, " ") }

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}

func symlinkFile(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

// setDevicePathForTest points devicePathFunc at fn for the duration of a
// test, returning a restore func -- see devicePathFunc's own doc comment.
func setDevicePathForTest(fn func(iscsiConfig) string) (restore func()) {
	prev := devicePathFunc
	devicePathFunc = fn
	return func() { devicePathFunc = prev }
}

// setDeviceWaitTimeoutForTest shrinks deviceWaitTimeout/deviceWaitInterval
// so a test exercising the "device never appeared" path doesn't have to
// wait out the real 10s production timeout.
func setDeviceWaitTimeoutForTest() (restore func()) {
	prevTimeout, prevInterval := deviceWaitTimeout, deviceWaitInterval
	deviceWaitTimeout = 50 * time.Millisecond
	deviceWaitInterval = 5 * time.Millisecond
	return func() { deviceWaitTimeout, deviceWaitInterval = prevTimeout, prevInterval }
}
