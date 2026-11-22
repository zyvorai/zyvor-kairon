// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/fencing"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func (e *Engine) redfishClient(ctx context.Context, t RedfishTarget) (*http.Client, string, string, error) {
	allowed := false
	for _, origin := range e.RedfishOrigins {
		if t.Endpoint == origin {
			allowed = true
		}
	}
	if !allowed {
		return nil, "", "", fmt.Errorf("the Redfish origin is not in the administrator allowlist")
	}
	secret, err := e.Kube.GetSecret(ctx, e.ControlNamespace, t.SecretName)
	if err != nil {
		return nil, "", "", err
	}
	value := func(key string) (string, error) {
		return string(secret.Data[key]), nil
	}
	user, err := value("username")
	if err != nil || user == "" {
		return nil, "", "", fmt.Errorf("missing Redfish username")
	}
	password, err := value("password")
	if err != nil || password == "" {
		return nil, "", "", fmt.Errorf("missing Redfish password")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if len(secret.Data["ca.crt"]) > 0 {
		ca, err := value("ca.crt")
		if err != nil || !roots.AppendCertsFromPEM([]byte(ca)) {
			return nil, "", "", fmt.Errorf("invalid Redfish CA")
		}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("redirects from the Redfish endpoint are forbidden")
	}}
	return client, user, password, nil
}
func redfishRequest(ctx context.Context, client *http.Client, endpoint, method, user, password string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(user, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the Redfish request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("the Redfish endpoint returned HTTP %d", resp.StatusCode)
	}
	if method != http.MethodGet {
		return "", nil
	}
	var state struct {
		PowerState string `json:"PowerState"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&state); err != nil {
		return "", fmt.Errorf("invalid Redfish response")
	}
	return state.PowerState, nil
}
func (e *Engine) fenceNode(ctx context.Context, o *model.FleetResource, nodes []model.Node) error {
	if o.Metadata.Namespace != e.ControlNamespace {
		return fmt.Errorf("fence requests are restricted to the control namespace")
	}
	s, _ := decode[FenceSpec](o.Spec)
	var node *model.Node
	for i := range nodes {
		if nodes[i].Metadata.Name == s.Node {
			node = &nodes[i]
			break
		}
	}
	if node == nil || node.Metadata.UID != s.NodeUID {
		return fmt.Errorf("node identity changed or disappeared")
	}
	if o.Status.Phase == "Succeeded" {
		return nil
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			return fmt.Errorf("refusing to power-fence a Ready node")
		}
	}
	profile, err := e.Kube.GetFleet(ctx, e.ControlNamespace, "machinehaprofiles", s.ProfileName)
	if err != nil {
		return err
	}
	if err := Validate(profile); err != nil {
		return err
	}
	p, _ := decode[HAProfileSpec](profile.Spec)
	target, ok := p.Nodes[s.Node]
	if !ok {
		return fmt.Errorf("no fencing target for node")
	}
	client, user, password, err := e.redfishClient(ctx, target)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	endpoint := target.Endpoint + "/redfish/v1/Systems/" + url.PathEscape(target.SystemID)
	// Cordon before contacting the BMC; never schedule back onto a fenced host.
	if err := e.Kube.PatchNode(ctx, s.Node, map[string]any{"metadata": map[string]any{"resourceVersion": node.Metadata.ResourceVersion}, "spec": map[string]any{"unschedulable": true}}); err != nil {
		return err
	}
	state, err := redfishRequest(ctx, client, endpoint, http.MethodGet, user, password, nil)
	if err != nil {
		return err
	}
	if state != "Off" {
		// Repeated ForceOff is idempotent; an ambiguous HTTP result does not prove fencing.
		if err := func() error {
			_, err := redfishRequest(ctx, client, endpoint+"/Actions/ComputerSystem.Reset", http.MethodPost, user, password, map[string]string{"ResetType": "ForceOff"})
			return err
		}(); err != nil {
			return err
		}
		e.status(o, "WaitingForPowerOff", "ForceOff requested; awaiting an observed Off response")
		return nil
	}
	now := e.now()
	e.status(o, "Succeeded", "Redfish confirmed Off for node UID "+s.NodeUID)
	o.Status.LastActionTime = &now
	return nil
}
func (e *Engine) highAvailability(ctx context.Context, o *model.FleetResource, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) error {
	if o.Metadata.Namespace != e.ControlNamespace {
		return fmt.Errorf("HA profiles are restricted to the control namespace")
	}
	spec, _ := decode[HAProfileSpec](o.Spec)
	byNode := map[string]model.Node{}
	for _, n := range nodes {
		byNode[n.Metadata.Name] = n
	}
	for _, m := range machines {
		// An explicit per-Machine opt-in names the trusted control-namespace profile.
		if m.Metadata.Annotations[HAProfileAnnotation] != o.Metadata.Name || !model.LabelsMatch(m.Metadata.Labels, spec.Selector) || m.Spec.NodeName == "" || m.DesiredPowerState() != "Running" || m.Metadata.DeletionTimestamp != nil || hasMigration(m, migrations) {
			continue
		}
		n, ok := byNode[m.Spec.NodeName]
		if !ok {
			continue
		}
		failedSince := time.Time{}
		for _, c := range m.Status.Conditions {
			if c.Type == model.ConditionNodeUnreachable && c.Status == "True" {
				failedSince = c.LastTransitionTime
			}
		}
		if failedSince.IsZero() || e.now().Sub(failedSince) < time.Duration(spec.FailureGraceSeconds)*time.Second {
			continue
		}
		attempt, err := strconv.Atoi(m.Metadata.Annotations["fleet.kairon.zyvor.dev/restart-count"])
		if err != nil {
			attempt = 0
		}
		if attempt >= spec.MaxRestarts {
			continue
		}
		requestName := childName(*o, "fence/"+n.Metadata.UID)
		req, err := e.Kube.GetFleet(ctx, e.ControlNamespace, "nodefencerequests", requestName)
		if kube.IsNotFound(err) {
			_, err = e.Kube.CreateFleet(ctx, e.ControlNamespace, "nodefencerequests", model.FleetResource{TypeMeta: model.TypeMeta{APIVersion: model.FleetAPIVersion, Kind: "NodeFenceRequest"}, Metadata: childMeta(*o, requestName), Spec: raw(FenceSpec{Node: n.Metadata.Name, NodeUID: n.Metadata.UID, ProfileName: o.Metadata.Name})})
			if err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !owns(*o, req.Metadata) {
			return fmt.Errorf("foreign fence request")
		}
		if req.Status.Phase != "Succeeded" || req.Status.LastActionTime == nil {
			continue
		}
		fs, err := decode[FenceSpec](req.Spec)
		if err != nil || fs.NodeUID != n.Metadata.UID || fs.Node != n.Metadata.Name {
			return fmt.Errorf("fence evidence identity mismatch")
		}
		// Confirm Off again immediately before releasing VM ownership; never trust old evidence alone.
		target, ok := spec.Nodes[n.Metadata.Name]
		if !ok {
			return fmt.Errorf("missing fencing target")
		}
		client, user, password, err := e.redfishClient(ctx, target)
		if err != nil {
			return err
		}
		state, readErr := redfishRequest(ctx, client, target.Endpoint+"/redfish/v1/Systems/"+url.PathEscape(target.SystemID), http.MethodGet, user, password, nil)
		client.CloseIdleConnections()
		if readErr != nil {
			return readErr
		}
		if state != "Off" {
			return fmt.Errorf("node is no longer powered off")
		}
		if err := e.disrupt(m); err != nil {
			return err
		}
		if err := e.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": map[string]any{"resourceVersion": m.Metadata.ResourceVersion, "annotations": map[string]string{"fleet.kairon.zyvor.dev/restart-count": strconv.Itoa(attempt + 1)}}}); err != nil {
			return err
		}
		if err := fencing.Fence(ctx, e.Kube, m, "VerifiedRedfishOff", "verified node UID "+n.Metadata.UID+" through "+requestName); err != nil {
			return err
		}
		now := e.now()
		o.Status.LastActionTime = &now
	}
	e.status(o, "Ready", "HA watches opted-in Machines; fenced hosts remain cordoned")
	return nil
}

// RedfishOrigins parses configuration only, never tenant-authored endpoints.
func RedfishOrigins(value string) []string {
	var out []string
	for _, s := range strings.Split(value, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
