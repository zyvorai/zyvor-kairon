// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package migration

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzSessionValidate(f *testing.F) {
	f.Add("a1b2c3", "prod", "db", "node-1", "node-2", "")
	f.Add("../x", "p", "m", "n1", "n2", "net")
	f.Add("", "", "", "", "", "")
	f.Fuzz(func(t *testing.T, id, ns, machine, src, dst, network string) {
		s := Session{ID: id, Namespace: ns, Machine: machine, SourceNode: src, TargetNode: dst, MigrationNetwork: network}
		if err := s.Validate(); err != nil {
			return
		}
		// A session that validates must never be able to address a file
		// outside the store directory.
		dir := t.TempDir()
		p, err := NewFileStore(dir).path(s.ID)
		if err != nil {
			return
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("session id %q escapes the store: %s", s.ID, p)
		}
	})
}

func FuzzSessionJSON(f *testing.F) {
	f.Add([]byte(`{"id":"a1","namespace":"prod","machine":"db","sourceNode":"n1","targetNode":"n2"}`))
	f.Add([]byte(`{"stream":{"multifdChannels":300,"compression":{"algorithm":"zstd","level":999}}}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var s Session
		if json.Unmarshal(raw, &s) != nil {
			return
		}
		_ = s.Validate()
		_ = s.SameIdentity(s)
	})
}
