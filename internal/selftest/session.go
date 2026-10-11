// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package selftest

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// Pinned CirrOS 0.6.3 images, from https://download.cirros-cloud.net/0.6.3/SHA256SUMS.
// kairon-node refuses an image whose sha256 does not match, so a wrong or
// moved mirror fails loudly instead of booting something else.
type pinnedImage struct{ URL, Digest string }

var cirros = map[string]pinnedImage{
	"amd64": {
		URL:    "https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-x86_64-disk.img",
		Digest: "sha256:7d6355852aeb6dbcd191bcda7cd74f1536cfe5cbf8a10495a7283a8396e4b75b",
	},
	"arm64": {
		URL:    "https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-aarch64-disk.img",
		Digest: "sha256:611879b8299363fe60ff0f84982e4da08e63f476e8481170223db982959783cb",
	},
}

// session is the state shared by the scenarios of one run.
type session struct {
	env *Env
	rep Reporter

	runID string
	arch  string
	// created lists every Machine to delete at the end, in creation order.
	created []string

	// vm is the primary CirrOS Machine, set by the boot scenario.
	vm       string
	node     string
	hostPort int

	// hint and skipReason are set by a scenario for its Result.
	hint       string
	skipReason string
}

func newSession(env *Env, rep Reporter) *session {
	id := env.RunID
	if id == "" {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
	}
	return &session{env: env, rep: rep, runID: id}
}

func (s *session) logf(l Level, format string, a ...any) { s.rep.Log(l, fmt.Sprintf(format, a...)) }

// randomPort picks a high TCP port for the guest SSH forward.
func randomPort() int {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return 20000 + int(binary.BigEndian.Uint16(b))%40000
}

// imageSpec resolves the boot image for the run.
func (s *session) imageSpec() (model.ImageSpec, error) {
	e := s.env
	switch {
	case e.ImagePath != "":
		return model.ImageSpec{Path: e.ImagePath}, nil
	case e.ImageURL != "":
		if e.ImageDigest == "" {
			return model.ImageSpec{}, fmt.Errorf("--image-url needs --image-digest (the node verifies it)")
		}
		return model.ImageSpec{Digest: e.ImageDigest, Source: &model.ImageSource{HTTPURL: e.ImageURL, Format: "qcow2"}}, nil
	}
	p, ok := cirros[s.arch]
	if !ok {
		return model.ImageSpec{}, fmt.Errorf("no pinned CirrOS image for architecture %q; pass --image PATH or --image-url/--image-digest", s.arch)
	}
	return model.ImageSpec{Digest: p.Digest, Source: &model.ImageSource{HTTPURL: p.URL, Format: "qcow2"}}, nil
}

// detectArch picks the architecture of a capable, Ready node.
func (s *session) detectArch(ctx context.Context) {
	if s.env.Arch != "" {
		s.arch = s.env.Arch
		return
	}
	s.arch = "amd64"
	nodes, err := s.env.Kube.ListNodes(ctx)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if n.Metadata.Labels[model.CapableLabel] == "true" && nodeReady(n) {
			if a := n.Metadata.Labels["kubernetes.io/arch"]; a != "" {
				s.arch = a
				return
			}
		}
	}
}

func nodeReady(n model.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// capableNodes lists Ready nodes carrying the capable label.
func (s *session) capableNodes(ctx context.Context) ([]model.Node, error) {
	nodes, err := s.env.Kube.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Node
	for _, n := range nodes {
		if n.Metadata.Labels[model.CapableLabel] == "true" && nodeReady(n) && !n.Spec.Unschedulable {
			out = append(out, n)
		}
	}
	return out, nil
}

// newMachine builds a test Machine: 1 vCPU, 256 MiB, user networking with a
// forward to guest port 22, labelled and TTL-bounded so a killed run cannot
// leak it forever.
func (s *session) newMachine(role string, img model.ImageSpec) model.Machine {
	name := fmt.Sprintf("kairon-selftest-%s-%s", s.runID, role)
	if len(name) > 52 { // leave room for FluxVM's runtime-name suffix
		name = name[:52]
	}
	return model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{
			Name: name, Namespace: s.env.Namespace,
			Labels: map[string]string{LabelKey: s.runID, "kairon.zyvor.dev/selftest-role": role},
		},
		Spec: model.MachineSpec{
			Image:      img,
			Resources:  model.ResourceSpec{CPU: "1", Memory: "256Mi"},
			Runtime:    model.RuntimeSpec{Backend: "qemu"},
			Network:    model.NetworkSpec{Mode: "user"},
			Placement:  model.PlacementSpec{Architecture: s.arch},
			PowerState: "Running",
			TTLSeconds: int64((s.env.BootTimeout + 10*time.Minute).Seconds()),
		},
	}
}

func (s *session) create(ctx context.Context, m model.Machine) error {
	if _, err := s.env.Kube.CreateMachine(ctx, m.Metadata.Namespace, m); err != nil {
		return err
	}
	s.created = append(s.created, m.Metadata.Name)
	return nil
}

func (s *session) get(ctx context.Context, name string) (model.Machine, error) {
	return s.env.Kube.GetMachine(ctx, s.env.Namespace, name)
}

func (s *session) patch(ctx context.Context, name string, patch map[string]any) error {
	return s.env.Kube.PatchMachine(ctx, s.env.Namespace, name, patch)
}

// waitFor polls fn until it reports done, returns an error, or timeout.
func (s *session) waitFor(ctx context.Context, what string, timeout time.Duration, fn func() (bool, error)) error {
	deadline := s.env.Now().Add(timeout)
	for {
		done, err := fn()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if s.env.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout.Round(time.Second), what)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted while waiting for %s", what)
		case <-time.After(s.env.PollInterval):
		}
	}
}

// waitPhase waits for the Machine's status.phase to equal want. A Failed or
// Error phase ends the wait early with the controller's message.
func (s *session) waitPhase(ctx context.Context, name, want string, timeout time.Duration) (model.Machine, error) {
	var last model.Machine
	err := s.waitFor(ctx, fmt.Sprintf("%s to be %s (phase %q)", name, want, last.Status.Phase), timeout, func() (bool, error) {
		m, err := s.get(ctx, name)
		if err != nil {
			return false, nil // transient API errors are retried until the deadline
		}
		last = m
		switch m.Status.Phase {
		case want:
			return true, nil
		case "Failed", "Error":
			return false, fmt.Errorf("%s is %s: %s", name, m.Status.Phase, m.Status.Message)
		}
		return false, nil
	})
	if err != nil && last.Status.Phase != "" && strings.Contains(err.Error(), "timed out") {
		err = fmt.Errorf("%w; last phase %q: %s", err, last.Status.Phase, last.Status.Message)
	}
	return last, err
}

// serial returns the guest console text, or "" when it is unavailable.
func (s *session) serial(ctx context.Context, name string) string {
	if s.env.SerialLog == nil {
		return ""
	}
	out, err := s.env.SerialLog(ctx, s.env.Namespace, name)
	if err != nil {
		return ""
	}
	return out
}

// cleanup deletes every created Machine and waits for them to disappear.
func (s *session) cleanup() {
	if len(s.created) == 0 {
		return
	}
	if s.env.Keep {
		s.logf(LevelWarn, "--keep: leaving %s in namespace %s (delete with: kaironctl delete machine NAME -n %s)", strings.Join(s.created, ", "), s.env.Namespace, s.env.Namespace)
		return
	}
	// A fresh context: the run's may already be cancelled (Ctrl-C).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := len(s.created) - 1; i >= 0; i-- {
		name := s.created[i]
		if err := s.env.Kube.DeleteMachine(ctx, s.env.Namespace, name); err != nil && !kube.IsNotFound(err) {
			s.logf(LevelWarn, "could not delete %s: %v (kaironctl delete machine %s -n %s)", name, err, name, s.env.Namespace)
			continue
		}
	}
	for _, name := range s.created {
		for {
			if _, err := s.env.Kube.GetMachine(ctx, s.env.Namespace, name); kube.IsNotFound(err) {
				s.logf(LevelOK, "cleaned up %s", name)
				break
			}
			select {
			case <-ctx.Done():
				s.logf(LevelWarn, "%s still exists; find leftovers with: kaironctl get machines -n %s --selector %s=%s", name, s.env.Namespace, LabelKey, s.runID)
				goto next
			case <-time.After(s.env.PollInterval):
			}
		}
	next:
	}
	s.created = nil
}
