// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

// Package agentplane is the AI-agent control surface for Kairon.
//
// Controllers stay deterministic. This package proposes: it compiles a
// strict egress allowlist, explains eBPF drops, refuses an agent claim
// that is not sealed, and records an audit event an agent can replay.
// Nothing here talks to the apiserver or to a model. Admission and MCP
// call these functions and keep the apply path outside the model.
