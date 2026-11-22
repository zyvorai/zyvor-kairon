// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/model"
)

// ImageVerifier checks that ref's repository holds a trusted signature
// for digest. *cosign.Verifier implements it.
type ImageVerifier interface {
	Verify(ctx context.Context, ref, digest string) error
}

const imageVerifyTimeout = 8 * time.Second

// verifyAgentImage enforces the image signature for agent-pool Machines
// when a verifier is configured. Other Machines are never checked.
func (c *Controller) verifyAgentImage(ctx context.Context, m model.Machine) error {
	if c.Cosign == nil {
		return nil
	}
	v := strings.ToLower(strings.TrimSpace(m.Metadata.Annotations[agentplane.AnnAgentPool]))
	if v != "true" && v != "1" && v != "yes" {
		return nil
	}
	src := m.Spec.Image.Source
	if src == nil || src.OCI == "" {
		return fmt.Errorf("agent pool image must be spec.image.source.oci so its cosign signature can be verified")
	}
	ctx, cancel := context.WithTimeout(ctx, imageVerifyTimeout)
	defer cancel()
	if err := c.Cosign.Verify(ctx, src.OCI, m.Spec.Image.Digest); err != nil {
		return fmt.Errorf("agent pool image signature: %w", err)
	}
	return nil
}
