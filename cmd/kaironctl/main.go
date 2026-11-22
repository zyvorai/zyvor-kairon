// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package main

import (
	"os"

	"github.com/zyvorai/kairon/internal/kaironctl"
)

// version is injected via -ldflags -X main.version=... (Makefile/
// Dockerfile) -- kept local to this package rather than moved into
// internal/kaironctl so that existing build convention needs no change;
// cmd/kubectl-kairon has its own identical copy for the same reason.
var version = "dev"

func main() {
	os.Exit(kaironctl.RunAs("kaironctl", os.Args[1:], version))
}
