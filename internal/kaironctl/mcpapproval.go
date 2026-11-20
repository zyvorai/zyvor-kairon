// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/mcp"
)

// AnnotationMCPApproval holds one pending human approval for one MCP tool
// call on the annotated object: "<id>;<expires RFC3339>;<approver>".
const AnnotationMCPApproval = "kairon.zyvor.dev/mcp-approval"

const defaultApprovalTTL = 10 * time.Minute

// approvalKinds maps the KIND accepted by `kaironctl approve` to its
// resource plural.
var approvalKinds = map[string]string{"machine": "machines", "machinebackup": "machinebackups"}

type approvalTarget struct {
	Kind, Namespace, Name string
}

func (t approvalTarget) String() string { return t.Kind + "/" + t.Namespace + "/" + t.Name }

// mcpApprovalTarget names the object whose annotation approves this call,
// or ok=false when the call needs no approval (machine_backup create).
func mcpApprovalTarget(tool string, args json.RawMessage, nsDefault string) (approvalTarget, bool, error) {
	var a struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Action    string `json:"action"`
		Backup    string `json:"backup"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return approvalTarget{}, false, fmt.Errorf("invalid arguments: %w", err)
	}
	ns := machineRef{Namespace: a.Namespace}.ns(nsDefault)
	switch tool {
	case "delete_machine", "fork_machine":
		if a.Name == "" {
			return approvalTarget{}, false, fmt.Errorf("name is required")
		}
		return approvalTarget{"machine", ns, a.Name}, true, nil
	case "machine_backup":
		if a.Action != "restore" && a.Action != "delete" {
			return approvalTarget{}, false, nil
		}
		if a.Backup == "" {
			return approvalTarget{}, false, fmt.Errorf("backup is required for %s", a.Action)
		}
		return approvalTarget{"machinebackup", ns, a.Backup}, true, nil
	}
	return approvalTarget{}, false, fmt.Errorf("tool %s has no approval target", tool)
}

// mcpApprovalID is stable for the same tool, arguments (key order does not
// matter) and principal, and changes if any of them does.
func mcpApprovalID(tool string, args json.RawMessage, principal string) (string, error) {
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(tool + "\n" + string(canon) + "\n" + principal))
	return hex.EncodeToString(sum[:])[:12], nil
}

func formatApproval(id string, expires time.Time, approver string) string {
	return id + ";" + expires.UTC().Format(time.RFC3339) + ";" + approver
}

func parseApproval(v string) (id string, expires time.Time, approver string, ok bool) {
	parts := strings.SplitN(v, ";", 3)
	if len(parts) != 3 {
		return "", time.Time{}, "", false
	}
	exp, err := time.Parse(time.RFC3339, parts[1])
	if err != nil {
		return "", time.Time{}, "", false
	}
	return parts[0], exp, parts[2], true
}

// approvalStore is the slice of kube.Client the approval check needs.
type approvalStore interface {
	GetAnnotations(ctx context.Context, resource, ns, name string) (map[string]string, error)
	ConsumeAnnotation(ctx context.Context, resource, ns, name, key, value string) error
}

// mcpApproveHook checks and consumes a human approval for each Approval
// tool call. The approval is spent before the tool runs, so a failed call
// needs a fresh one.
func mcpApproveHook(nsDefault string, newStore func() (approvalStore, error), now func() time.Time) func(context.Context, string, json.RawMessage) (string, error) {
	return func(ctx context.Context, tool string, args json.RawMessage) (string, error) {
		target, needed, err := mcpApprovalTarget(tool, args, nsDefault)
		if err != nil {
			return "", err
		}
		if !needed {
			return "", nil
		}
		id, err := mcpApprovalID(tool, args, mcpPrincipal())
		if err != nil {
			return "", err
		}
		required := &mcp.ApprovalError{
			Outcome: "approval-required",
			Message: fmt.Sprintf("%s needs a human's approval (id %s). Ask them to run:\n  kaironctl approve %s %s\nthen call %s again with exactly the same arguments.", tool, id, target, id, tool),
		}
		store, err := newStore()
		if err != nil {
			return "", err
		}
		resource := approvalKinds[target.Kind]
		ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
		defer cancel()
		ann, err := store.GetAnnotations(ctx, resource, target.Namespace, target.Name)
		if err != nil {
			return "", err
		}
		value := ann[AnnotationMCPApproval]
		gotID, expires, approver, ok := parseApproval(value)
		if !ok || gotID != id {
			return "", required
		}
		if now().After(expires) {
			return "", &mcp.ApprovalError{
				Outcome: "approval-expired",
				Message: fmt.Sprintf("the approval for %s (id %s) expired at %s; ask for a new one:\n  kaironctl approve %s %s", tool, id, expires.Format(time.RFC3339), target, id),
			}
		}
		if err := store.ConsumeAnnotation(ctx, resource, target.Namespace, target.Name, AnnotationMCPApproval, value); err != nil {
			if errors.Is(err, kube.ErrAnnotationChanged) {
				return "", required
			}
			return "", err
		}
		return approver, nil
	}
}

func approverName() string {
	if p := strings.TrimSpace(os.Getenv("KAIRON_APPROVER")); p != "" {
		return p
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func newApproveCmd(opts *Options) *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "approve KIND/[NAMESPACE/]NAME ID",
		Short: "Approve one pending MCP agent action",
		Long: `Approves exactly one destructive MCP tool call (delete_machine,
fork_machine, machine_backup restore or delete). The agent's refused call
prints the KIND/NAMESPACE/NAME and ID to pass here. The approval is
written as the kairon.zyvor.dev/mcp-approval annotation on that object,
is used up by the agent's next identical call, and expires after --ttl.
KIND is machine or machinebackup. The approver is $KAIRON_APPROVER, else
the local user name.`,
		Example: `  kaironctl approve machine/prod/web-1 3f9a1c0b7d2e`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			parts := strings.Split(args[0], "/")
			var target approvalTarget
			switch len(parts) {
			case 2:
				target = approvalTarget{parts[0], opts.Namespace, parts[1]}
			case 3:
				target = approvalTarget{parts[0], parts[1], parts[2]}
			default:
				return fmt.Errorf("want KIND/NAMESPACE/NAME or KIND/NAME, got %q", args[0])
			}
			resource, ok := approvalKinds[target.Kind]
			if !ok || target.Name == "" || target.Namespace == "" {
				return fmt.Errorf("KIND must be machine or machinebackup, with a name: %q", args[0])
			}
			id := args[1]
			if len(id) != 12 || strings.Trim(id, "0123456789abcdef") != "" {
				return fmt.Errorf("ID %q is not an approval id (12 hex characters)", id)
			}
			if ttl <= 0 {
				return fmt.Errorf("--ttl must be positive")
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			expires := time.Now().Add(ttl)
			if err := kc.SetAnnotation(cmd.Context(), resource, target.Namespace, target.Name, AnnotationMCPApproval, formatApproval(id, expires, approverName())); err != nil {
				return err
			}
			okf("approved %s on %s until %s", id, target, expires.Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", defaultApprovalTTL, "how long the approval stays usable")
	return cmd
}
