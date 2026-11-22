// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// kubectl-kairon is a real kubectl plugin, not a shim that shells out to
// a separately-installed kaironctl -- it shares kaironctl's exact command
// dispatch (internal/kaironctl) directly, since kubectl already invokes a
// plugin binary with the subcommand as its own os.Args[1:] (`kubectl
// kairon evacuate worker-1` runs `kubectl-kairon evacuate worker-1`),
// exactly the argument shape kaironctl's own Run already expects. Install
// by putting this binary on $PATH as `kubectl-kairon` (see
// https://krew.sigs.k8s.io/docs/developer-guide/ for the kubectl plugin
// convention this follows) -- then `kubectl kairon ...` works exactly
// like `kaironctl ...`.
package main

import (
	"os"

	"github.com/zyvorai/kairon/internal/kaironctl"
)

// version is injected via -ldflags -X main.version=... (Makefile/
// Dockerfile), the same convention cmd/kaironctl's own copy uses.
var version = "dev"

func main() {
	os.Exit(kaironctl.RunAs("kubectl kairon", os.Args[1:], version))
}
