// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package kaironctl

// notifyWindowResize does nothing on Windows, which has no SIGWINCH; the
// terminal keeps the size it started with.
func notifyWindowResize(func()) (stop func()) { return func() {} }
