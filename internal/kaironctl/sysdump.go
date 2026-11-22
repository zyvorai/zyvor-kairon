// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
)

const redacted = "<redacted>"

// secretKey matches map keys whose values must never leave the cluster in a
// support bundle: credentials, tokens, key material, password hashes.
var secretKey = regexp.MustCompile(`(?i)(password|passwd|token|api[-_]?key|private[-_]?key|credential|secret(?:key|data)?$|hash|cabundle|client[-_]?secret)`)

// redactValue returns v with sensitive values replaced. It never mutates v.
//   - any value under a key matching secretKey becomes "<redacted>";
//   - an object whose kind is Secret keeps its metadata but loses data/stringData;
//   - a Machine's spec.cloudInit (runCmd/writeFiles may embed credentials) is
//     reduced to a marker.
func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		isSecret := x["kind"] == "Secret"
		// Container env entries and similar name/value pairs: the secret-ness
		// is in the name ("KAIRON_UI_TOKEN"), not in the "value" key.
		nameIsSecret := false
		if n, ok := x["name"].(string); ok {
			nameIsSecret = secretKey.MatchString(n)
		}
		for k, val := range x {
			switch {
			case nameIsSecret && k == "value":
				out[k] = redacted
			case isSecret && (k == "data" || k == "stringData"):
				out[k] = redacted
			case k == "cloudInit":
				out[k] = map[string]any{"redacted": true}
			case secretKey.MatchString(k):
				if _, isStr := val.(string); isStr || val == nil {
					out[k] = redacted
				} else {
					out[k] = redactAll(val) // structured value: keep the shape, redact every scalar
				}
			default:
				out[k] = redactValue(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactValue(e)
		}
		return out
	}
	return v
}

// redactAll replaces every scalar below v with "<redacted>", keeping the shape.
func redactAll(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = redactAll(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactAll(e)
		}
		return out
	case nil:
		return nil
	}
	return redacted
}

// redactJSON redacts a JSON document; non-JSON input is returned unchanged.
func redactJSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	b, err := json.MarshalIndent(redactValue(v), "", "  ")
	if err != nil {
		return raw
	}
	return b
}

var secretDoc = regexp.MustCompile(`(?m)^kind:\s*Secret\s*$`)

// redactManifest drops Secret documents from a rendered Helm manifest.
func redactManifest(manifest string) string {
	docs := strings.Split(manifest, "\n---")
	for i, d := range docs {
		if secretDoc.MatchString(d) {
			docs[i] = "\n# Secret document omitted from the support bundle\n"
		}
	}
	return strings.Join(docs, "\n---")
}

type bundleWriter struct {
	tw   *tar.Writer
	when time.Time
	n    int
	skip []string
}

func (b *bundleWriter) add(name string, data []byte) error {
	if err := b.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: b.when}); err != nil {
		return err
	}
	_, err := b.tw.Write(data)
	b.n++
	return err
}

// addOptional records a collection failure in the bundle instead of aborting:
// a support bundle is most needed when part of the cluster is broken.
func (b *bundleWriter) addOptional(name string, data []byte, err error) error {
	if err != nil {
		b.skip = append(b.skip, fmt.Sprintf("%s: %v", name, err))
		return nil
	}
	return b.add(name, data)
}

type sysdumpOpts struct {
	Namespace   string
	ReleaseName string
	Output      string
	Tail        int
	NoLogs      bool
	NoCRs       bool
}

func newSysdumpCmd(opts *Options) *cobra.Command {
	o := &sysdumpOpts{Namespace: "kairon-system", ReleaseName: "kairon", Tail: 1000}
	cmd := &cobra.Command{
		Use:   "sysdump",
		Short: "Collect a redacted support bundle (tar.gz) for troubleshooting",
		Long: `Collect Helm values and manifest, workload and pod specs, events, node objects, Kairon
resources, doctor results and recent pod logs into one tar.gz.

Redaction: values under keys that look like credentials (password, token, key, hash,
caBundle) and Secret data are replaced with "<redacted>"; Machine spec.cloudInit is
omitted; Secret documents are dropped from the rendered manifest. Review the bundle
before sharing it: logs are included as written by the components.`,
		Example: `  $ kaironctl sysdump
  $ kaironctl sysdump -o /tmp/kairon.tar.gz --tail 5000
  $ kaironctl sysdump --no-logs --no-crs`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if o.Output == "" {
				o.Output = "kairon-sysdump-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
			}
			f, err := os.OpenFile(o.Output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			n, skipped, err := writeSysdump(ctx, kc, f, o, opts.Version)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(o.Output)
				return err
			}
			style.Log(style.EmojiOK, "Wrote %s (%d files, %d skipped)", o.Output, n, len(skipped))
			for _, s := range skipped {
				style.Warnf("skipped %s", s)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&o.Namespace, "namespace", "n", o.Namespace, "namespace where Kairon is installed")
	cmd.Flags().StringVar(&o.ReleaseName, "helm-release-name", o.ReleaseName, "Helm release name")
	cmd.Flags().StringVarP(&o.Output, "output", "o", "", "output file (default kairon-sysdump-<UTC time>.tar.gz)")
	cmd.Flags().IntVar(&o.Tail, "tail", o.Tail, "log lines per container")
	cmd.Flags().BoolVar(&o.NoLogs, "no-logs", false, "do not collect pod logs")
	cmd.Flags().BoolVar(&o.NoCRs, "no-crs", false, "do not collect Kairon custom resources")
	return cmd
}

func writeSysdump(ctx context.Context, kc *kube.Client, w io.Writer, o *sysdumpOpts, clientVersion string) (int, []string, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	b := &bundleWriter{tw: tw, when: time.Now().UTC()}
	ref := &releaseRef{Name: o.ReleaseName, Namespace: o.Namespace}
	fail := func(err error) (int, []string, error) { return b.n, b.skip, err }

	// Environment and doctor.
	info := map[string]any{"client": clientVersion, "collected": b.when.Format(time.RFC3339), "namespace": o.Namespace}
	if sv, err := collectServerVersion(ctx, ref); err == nil {
		info["server"] = sv
	} else {
		info["serverError"] = err.Error()
	}
	infoJSON, _ := json.MarshalIndent(info, "", "  ")
	if err := b.add("version.json", infoJSON); err != nil {
		return fail(err)
	}
	rep := runDoctor(ctx, kc, &doctorOpts{Namespace: o.Namespace, ReleaseName: o.ReleaseName})
	repJSON, _ := json.MarshalIndent(rep, "", "  ")
	if err := b.add("doctor.json", repJSON); err != nil {
		return fail(err)
	}

	// Helm release.
	if vals, err := releaseValuesFn(ref, false); err == nil {
		y, _ := yaml.Marshal(redactValue(normalizeJSON(vals)))
		if err := b.add("helm/values.yaml", y); err != nil {
			return fail(err)
		}
	} else {
		b.skip = append(b.skip, fmt.Sprintf("helm/values.yaml: %v", err))
	}
	if rel, err := getReleaseFn(ref); err == nil && rel != nil {
		if err := b.add("helm/manifest.yaml", []byte(redactManifest(rel.Manifest))); err != nil {
			return fail(err)
		}
	}
	if revs, err := releaseHistoryFn(ref); err == nil {
		var buf bytes.Buffer
		_ = writeHistory(&buf, revs, "table")
		if err := b.add("helm/history.txt", buf.Bytes()); err != nil {
			return fail(err)
		}
	}

	// Namespace objects.
	for _, kind := range []string{"deployments", "daemonsets", "services", "pods"} {
		p := fmt.Sprintf("/apis/apps/v1/namespaces/%s/%s", o.Namespace, kind)
		if kind == "services" || kind == "pods" {
			p = fmt.Sprintf("/api/v1/namespaces/%s/%s", o.Namespace, kind)
		}
		raw, err := kubeGetObject(ctx, kc, p)
		if err := b.addOptional("k8s/"+kind+".json", redactJSON(raw), err); err != nil {
			return fail(err)
		}
	}
	rawEv, err := kubeGetObject(ctx, kc, fmt.Sprintf("/api/v1/namespaces/%s/events", o.Namespace))
	if err := b.addOptional("k8s/events.json", rawEv, err); err != nil {
		return fail(err)
	}
	rawNodes, err := kubeGetObject(ctx, kc, "/api/v1/nodes")
	if err := b.addOptional("k8s/nodes.json", redactJSON(rawNodes), err); err != nil {
		return fail(err)
	}
	rawWH, err := kubeGetObject(ctx, kc, "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/kairon-controller-webhook")
	if err != nil && kube.IsNotFound(err) {
		err = nil
		rawWH = nil
	}
	if rawWH != nil || err != nil {
		if err := b.addOptional("k8s/validatingwebhookconfiguration.json", redactJSON(rawWH), err); err != nil {
			return fail(err)
		}
	}

	// Kairon resources.
	if !o.NoCRs {
		for _, plural := range coreCRDs {
			raw, err := kubeGetObject(ctx, kc, "/apis/kairon.zyvor.dev/v1/"+plural)
			if err := b.addOptional("crs/"+plural+".json", redactJSON(raw), err); err != nil {
				return fail(err)
			}
		}
	}

	// Logs.
	if !o.NoLogs {
		pods, err := listPods(ctx, kc, o.Namespace, "")
		if err != nil {
			b.skip = append(b.skip, fmt.Sprintf("logs: %v", err))
		}
		for _, p := range pods {
			if !strings.HasPrefix(p.Name, "kairon-") {
				continue
			}
			for _, c := range p.Containers {
				for _, previous := range []bool{false, true} {
					if previous && p.Restarts == 0 {
						continue
					}
					name := path.Join("logs", p.Name, c+".log")
					if previous {
						name = path.Join("logs", p.Name, c+".previous.log")
					}
					rc, err := podLogs(ctx, kc, o.Namespace, p.Name, logOptions{Container: c, Tail: o.Tail, Previous: previous})
					if err != nil {
						b.skip = append(b.skip, fmt.Sprintf("%s: %v", name, err))
						continue
					}
					data, _ := io.ReadAll(io.LimitReader(rc, 16<<20))
					_ = rc.Close()
					if err := b.add(name, data); err != nil {
						return fail(err)
					}
				}
			}
		}
	}

	readme := "Kairon support bundle\n\nRedacted: secret-like keys, Secret data, Machine spec.cloudInit, Secret documents in helm/manifest.yaml.\nNot redacted: pod logs (as written by the components) and object names, labels and addresses.\n"
	if len(b.skip) > 0 {
		readme += "\nNot collected:\n- " + strings.Join(b.skip, "\n- ") + "\n"
	}
	if err := b.add("README.txt", []byte(readme)); err != nil {
		return fail(err)
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	return b.n, b.skip, nil
}

// normalizeJSON round-trips v through JSON so redaction sees plain maps.
func normalizeJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}
