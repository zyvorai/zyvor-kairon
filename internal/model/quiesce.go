// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import (
	"strings"
	"time"
)

// FormatQuiesceRef/ParseQuiesceRef encode/decode the "<MachineSnapshot
// name>@<RFC3339 time>" value AnnotationQuiesceRequest/
// AnnotationQuiesceStatus carry -- shared here rather than duplicated
// per-package (unlike most small cross-package helpers in this project,
// e.g. cmd/kairon-csi-node's own unixSocketPath), since kairon-controller
// and kairon-node must agree on this exact wire format to coordinate
// guest quiesce around a MachineSnapshot at all.
func FormatQuiesceRef(name string, at time.Time) string {
	return name + "@" + at.UTC().Format(time.RFC3339)
}

func ParseQuiesceRef(v string) (name string, at time.Time, ok bool) {
	name, ts, found := strings.Cut(v, "@")
	if !found {
		return "", time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "", time.Time{}, false
	}
	return name, t, true
}
