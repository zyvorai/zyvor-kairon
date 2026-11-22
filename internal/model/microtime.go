// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import (
	"encoding/json"
	"time"
)

// MicroTime marshals to/from Kubernetes' metav1.MicroTime wire format --
// RFC3339 with a fixed, zero-padded 6-digit fractional-second field, e.g.
// "2026-09-14T14:50:38.085612Z" -- which coordination.k8s.io/v1
// LeaseSpec's acquireTime/renewTime fields use (see internal/leaderelection),
// unlike the second-precision metav1.Time every other timestamp in this
// package's types uses (ObjectMeta.DeletionTimestamp among them). Plain
// time.Time's own MarshalJSON produces RFC3339Nano instead -- variable
// precision, trailing zeros trimmed -- which a real Kubernetes API server's
// strict MicroTime decoder rejects outright with a 400 (confirmed against
// a real cluster: every Lease write failed until this existed).
type MicroTime struct {
	time.Time
}

const microTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

func NewMicroTime(t time.Time) MicroTime {
	return MicroTime{Time: t}
}

func (t MicroTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Format(microTimeLayout))
}

// UnmarshalJSON parses with the more permissive time.RFC3339Nano layout
// (variable-width fractional seconds) rather than requiring exactly
// microTimeLayout's 6 digits back -- Go's "9"-pattern fractional-second
// parsing already accepts anywhere from 0 up to 9 digits, so this reads
// both our own MarshalJSON output and any other RFC3339 variant a real API
// server (or a test double) might send.
func (t *MicroTime) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}
