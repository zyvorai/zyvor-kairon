// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import "sync"

// Log is an append-only audit trail. Replay is the agent memory.
type Log struct {
	mu     sync.Mutex
	events []Event
}

func (l *Log) Append(e Event) (Event, error) {
	recorded, err := Record(e)
	if err != nil {
		return Event{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, recorded)
	return recorded, nil
}

func (l *Log) Replay(claim string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Replay(l.events, claim)
}
