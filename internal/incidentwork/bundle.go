// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package incidentwork binds a recorded incident to isolated recovery copies.
// Bundles are data only: nothing from the archive is extracted or executed.
package incidentwork

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/rehearsal"
)

type Manifest struct {
	Kind        string    `json:"kind"`
	Namespace   string    `json:"namespace"`
	Machine     string    `json:"machine"`
	UID         string    `json:"uid"`
	CollectedAt time.Time `json:"collectedAt"`
	Redacted    bool      `json:"redacted"`
}

// Bind validates a bounded incident archive and records its digest/identity in
// the plan. SourceUID is checked again against the actual MachineSnapshot.
func Bind(input io.Reader, p rehearsal.Plan) (rehearsal.Plan, error) {
	h := sha256.New()
	limited := io.LimitReader(input, 128<<20+1)
	raw, err := io.ReadAll(io.TeeReader(limited, h))
	if err != nil {
		return p, err
	}
	if len(raw) > 128<<20 {
		return p, fmt.Errorf("incident archive exceeds 128 MiB")
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return p, err
	}
	defer func() { _ = gz.Close() }()
	decoded, err := io.ReadAll(io.LimitReader(gz, 64<<20+1))
	if err != nil {
		return p, err
	}
	if len(decoded) > 64<<20 {
		return p, fmt.Errorf("incident contents exceed 64 MiB")
	}
	tr := tar.NewReader(bytes.NewReader(decoded))
	var manifest *Manifest
	var total int64
	for count := 0; ; count++ {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return p, err
		}
		if count > 10000 {
			return p, fmt.Errorf("too many archive entries")
		}
		if strings.Contains(header.Name, "\\") || strings.HasPrefix(header.Name, "/") || path.Clean(header.Name) != strings.TrimSuffix(header.Name, "/") || header.Name == ".." || strings.HasPrefix(header.Name, "../") {
			return p, fmt.Errorf("unsafe incident archive path")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return p, fmt.Errorf("incident archive must contain only regular files/directories")
		}
		if header.Size < 0 || header.Size > 64<<20 || total+header.Size > 64<<20 {
			return p, fmt.Errorf("incident contents exceed 64 MiB")
		}
		total += header.Size
		if header.Name == "manifest.json" {
			if manifest != nil || header.Size > 1<<20 {
				return p, fmt.Errorf("duplicate or oversized incident manifest")
			}
			var m Manifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return p, err
			}
			manifest = &m
		}
	}
	if manifest == nil || manifest.Kind != "KaironIncidentBundle" || !manifest.Redacted || manifest.Namespace != p.Namespace || manifest.Machine == "" || manifest.UID == "" || manifest.CollectedAt.IsZero() {
		return p, fmt.Errorf("a redacted Kairon incident manifest with matching namespace and source UID is required")
	}
	p.Purpose = "incident"
	p.Incident = &rehearsal.IncidentReference{SHA256: hex.EncodeToString(h.Sum(nil)), Machine: manifest.Machine, SourceUID: manifest.UID, CollectedAt: manifest.CollectedAt.UTC().Format(time.RFC3339)}
	return p, p.Validate()
}
