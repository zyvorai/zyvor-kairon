// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Event is one MCP write. Replay is the agent's memory and the
// operator's rollback log.
type Event struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Principal string    `json:"principal"`
	Tool      string    `json:"tool"`
	Tenant    string    `json:"tenant,omitempty"`
	Claim     string    `json:"claim,omitempty"`
	Diff      string    `json:"diff,omitempty"`
	// Outcome is "intent" before a write runs, then "ok" or the error.
	Outcome string `json:"outcome,omitempty"`
	// PrevHash and Hash chain FileLog records; empty for in-memory events.
	PrevHash string `json:"prevHash,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

func Record(e Event) (Event, error) {
	if strings.TrimSpace(e.Principal) == "" || strings.TrimSpace(e.Tool) == "" {
		return Event{}, fmt.Errorf("principal and tool are required")
	}
	if e.At.IsZero() {
		e.At = time.Unix(0, 0).UTC()
	}
	key := e.Principal + "|" + e.Tool + "|" + e.Tenant + "|" + e.Claim + "|" + e.Diff + "|" + e.At.UTC().Format(time.RFC3339Nano)
	if e.Outcome != "" {
		key += "|" + e.Outcome
	}
	sum := sha256.Sum256([]byte(key))
	e.ID = hex.EncodeToString(sum[:8])
	return e, nil
}

func Replay(events []Event, claim string) []Event {
	if claim == "" {
		return append([]Event(nil), events...)
	}
	out := make([]Event, 0)
	for _, e := range events {
		if e.Claim == claim {
			out = append(out, e)
		}
	}
	return out
}
