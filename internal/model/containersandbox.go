// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// OCISpec runs a container image as the Machine's sandbox on FluxVM's
// container track (POST /v1/sandboxes with an "oci" object): the image's
// root filesystem is mounted by a small init inside its own lightweight VM,
// with no Docker daemon and no shared Linux VM. FluxVM provides it on macOS
// hosts (Virtualization.framework); a node whose FluxVM cannot run it
// reports the error on the Machine.
//
// It excludes spec.image, spec.sandbox.templateName and spec.volumes, and
// is immutable after creation. Secret environment values are deliberately
// not part of this spec: a Machine object is readable by everyone who can
// read Machines, so a plaintext secret has no safe home here yet.
type OCISpec struct {
	// Image is the registry reference, e.g. "alpine:3.22",
	// "ghcr.io/org/app:1.2" or "nginx@sha256:...". Required.
	Image string `json:"image"`
	// Platform is "linux/arm64" (default) or "linux/amd64" (Rosetta).
	Platform string `json:"platform,omitempty"`
	// Command replaces the image's Cmd; Entrypoint replaces its Entrypoint
	// and drops Cmd, as docker run --entrypoint does.
	Command    []string `json:"command,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	// Env is KEY=value pairs added to or replacing the image's own.
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	// User is uid[:gid] or name[:group]; default is the image's User, else
	// an unprivileged one.
	User string `json:"user,omitempty"`
	// WritableRoot mounts the sandbox's own copy of the image read-write.
	WritableRoot bool `json:"writableRoot,omitempty"`
	// ExitPolicy is "keep" (the VM stays up for exec after the process
	// exits; the default) or "poweroff".
	ExitPolicy string `json:"exitPolicy,omitempty"`
	// Ports are published TCP ports, "HOST:CONTAINER" or "PORT" (host
	// ports 1024 and up); the node's 127.0.0.1:HOST reaches the container.
	Ports []string `json:"ports,omitempty"`
	// Restart is "no" (default), "on-failure" or "always".
	Restart     string `json:"restart,omitempty"`
	MaxRestarts *int32 `json:"maxRestarts,omitempty"`
	// HealthCheck runs a command in the container on an interval.
	HealthCheck *ContainerHealthCheck `json:"healthCheck,omitempty"`
	// Offline gives the container no network card; AllowHosts is an egress
	// allow-list (and implies offline). Neither combines with Ports.
	Offline    bool     `json:"offline,omitempty"`
	AllowHosts []string `json:"allowHosts,omitempty"`
	// Profile is FluxVM's sandbox size class (tiny, small, standard) used
	// when spec.resources is not set.
	Profile string `json:"profile,omitempty"`
}

// ContainerHealthCheck mirrors FluxVM's healthcheck object.
type ContainerHealthCheck struct {
	Command            []string `json:"command"`
	IntervalSeconds    uint32   `json:"intervalSeconds,omitempty"`
	TimeoutSeconds     uint32   `json:"timeoutSeconds,omitempty"`
	Retries            uint32   `json:"retries,omitempty"`
	StartPeriodSeconds uint32   `json:"startPeriodSeconds,omitempty"`
}

// ContainerStatus is the container track's view of the process, read from
// FluxVM's sandbox logs endpoint on each reconcile.
type ContainerStatus struct {
	// ExitCode is set once the container's process has exited.
	ExitCode *int32 `json:"exitCode,omitempty"`
	// Restarts counts process restarts made by the restart policy.
	Restarts uint32 `json:"restarts,omitempty"`
	// Health is "healthy" or "unhealthy" once a health check has run.
	Health string `json:"health,omitempty"`
	// InitError is set when the container's init could not start the process.
	InitError string `json:"initError,omitempty"`
}

// ValidateSandboxOCI rejects container sandbox specs FluxVM would refuse
// at create time, so the failure is an admission error, not a Machine stuck
// in Error.
func ValidateSandboxOCI(s MachineSpec) error {
	if s.Sandbox == nil || s.Sandbox.OCI == nil {
		return nil
	}
	o := s.Sandbox.OCI
	if strings.TrimSpace(o.Image) == "" {
		return fmt.Errorf("spec.sandbox.oci.image is required")
	}
	if strings.ContainsAny(o.Image, " \t\n") {
		return fmt.Errorf("spec.sandbox.oci.image %q must not contain whitespace", o.Image)
	}
	if s.Sandbox.TemplateName != "" {
		return fmt.Errorf("spec.sandbox.oci cannot be combined with spec.sandbox.templateName")
	}
	if s.Image.Path != "" || s.Image.Source != nil || s.Image.CatalogName != "" || s.Image.ImageRef != "" || s.Image.Blank {
		return fmt.Errorf("spec.sandbox.oci cannot be combined with spec.image: the container image is the root filesystem")
	}
	if len(s.Volumes) > 0 {
		return fmt.Errorf("spec.sandbox.oci cannot be combined with spec.volumes")
	}
	switch o.Platform {
	case "", "linux/arm64", "linux/amd64":
	default:
		return fmt.Errorf("spec.sandbox.oci.platform %q must be linux/arm64 or linux/amd64", o.Platform)
	}
	switch o.Restart {
	case "", "no", "on-failure", "always":
	default:
		return fmt.Errorf("spec.sandbox.oci.restart %q must be no, on-failure or always", o.Restart)
	}
	switch o.ExitPolicy {
	case "", "keep", "poweroff":
	default:
		return fmt.Errorf("spec.sandbox.oci.exitPolicy %q must be keep or poweroff", o.ExitPolicy)
	}
	if o.MaxRestarts != nil && *o.MaxRestarts < 0 {
		return fmt.Errorf("spec.sandbox.oci.maxRestarts must not be negative")
	}
	for i, e := range o.Env {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return fmt.Errorf("spec.sandbox.oci.env[%d] %q must be KEY=value", i, e)
		}
	}
	for i, p := range o.Ports {
		if err := validateContainerPort(p); err != nil {
			return fmt.Errorf("spec.sandbox.oci.ports[%d]: %w", i, err)
		}
	}
	if len(o.Ports) > 0 && (o.Offline || len(o.AllowHosts) > 0) {
		return fmt.Errorf("spec.sandbox.oci.ports cannot be combined with offline or allowHosts: published ports need the network card")
	}
	if o.HealthCheck != nil && len(o.HealthCheck.Command) == 0 {
		return fmt.Errorf("spec.sandbox.oci.healthCheck.command is required")
	}
	if s.Resources.Memory != "" {
		mib, err := ParseMemoryMiB(s.Resources.Memory)
		if err != nil {
			return fmt.Errorf("spec.resources.memory: %w", err)
		}
		if mib < 512 {
			return fmt.Errorf("spec.resources.memory must be at least 512Mi for a container sandbox")
		}
	}
	return nil
}

// validateContainerPort accepts "PORT", "HOST:CONTAINER", each optionally
// suffixed "/tcp"; host ports must be 1024 or higher.
func validateContainerPort(spec string) error {
	s := strings.TrimSpace(spec)
	if head, proto, ok := strings.Cut(s, "/"); ok {
		if !strings.EqualFold(proto, "tcp") {
			return fmt.Errorf("%q: only tcp can be published", spec)
		}
		s = head
	}
	host, guest, ok := strings.Cut(s, ":")
	if !ok {
		guest = host
	}
	h, errH := strconv.ParseUint(host, 10, 16)
	g, errG := strconv.ParseUint(guest, 10, 16)
	if errH != nil || errG != nil || h == 0 || g == 0 {
		return fmt.Errorf("%q is not PORT or HOST:CONTAINER", spec)
	}
	if h < 1024 {
		return fmt.Errorf("%q: host ports below 1024 cannot be published", spec)
	}
	return nil
}

// ValidateSandboxOCIUpdate rejects any change to spec.sandbox.oci after
// creation: the container is created once from it.
func ValidateSandboxOCIUpdate(old, cur MachineSpec) error {
	var o, c *OCISpec
	if old.Sandbox != nil {
		o = old.Sandbox.OCI
	}
	if cur.Sandbox != nil {
		c = cur.Sandbox.OCI
	}
	if !reflect.DeepEqual(o, c) {
		return fmt.Errorf("spec.sandbox.oci is immutable: delete and recreate the Machine to change the container")
	}
	return nil
}
