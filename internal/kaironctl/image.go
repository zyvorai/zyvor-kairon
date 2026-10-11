// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

type uploadedImage struct {
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	Format    string `json:"format,omitempty"`
	URL       string `json:"url"`
}

func newImageCmd(_ *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Upload disk images to kairon-ui's image store",
		Long: `Manage kairon-ui's content-addressed image store. Uploaded images are
served to kairon-node by digest and booted through spec.image.source.httpURL,
so they need node.imageCacheDir on the nodes and ui.imageStore.enabled in the
chart. Needs KAIRON_UI_URL and, for upload/delete, an admin KAIRON_UI_TOKEN.`,
	}
	cmd.AddCommand(newImageUploadCmd(), newImageListCmd(), newImageDeleteCmd())
	return cmd
}

func newImageUploadCmd() *cobra.Command {
	var name, format string
	var replace bool
	cmd := &cobra.Command{
		Use:   "upload FILE",
		Short: "Upload a qcow2/raw/ova/vmdk/vhd/vhdx file and print its spec.image",
		Example: `  kaironctl image upload ./noble.qcow2 --name ubuntu-24.04
  kaironctl image upload ./web01.ova --format ova`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				name = defaultImageName(args[0])
			}
			if format == "" {
				format = formatFromExt(args[0])
			}
			img, err := uploadImage(cmd.Context(), args[0], name, format, replace)
			if err != nil {
				return err
			}
			style.Log(style.EmojiOK, "image/%s uploaded (%s, %d bytes)", img.Name, img.Digest, img.SizeBytes)
			_, _ = fmt.Fprint(cmd.OutOrStdout(), imageSpecSnippet(img))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "image name (default: file name without extension, lowercased)")
	cmd.Flags().StringVar(&format, "format", "", "qcow2, raw, ova, vmdk, vhd or vhdx (default: from the file extension)")
	cmd.Flags().BoolVar(&replace, "replace", false, "repoint an existing name at the new upload")
	return cmd
}

func newImageListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List uploaded images",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var imgs []uploadedImage
			if err := uiJSON(cmd.Context(), http.MethodGet, "/api/v1/images", &imgs); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "NAME\tFORMAT\tSIZE\tDIGEST")
			for _, img := range imgs {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", img.Name, dash(img.Format), img.SizeBytes, img.Digest)
			}
			return tw.Flush()
		},
	}
}

func newImageDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete an uploaded image (nodes keep their cached copies)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := uiJSON(cmd.Context(), http.MethodDelete, "/api/v1/images/"+url.PathEscape(args[0]), nil); err != nil {
				return err
			}
			style.Log(style.EmojiOK, "image/%s deleted", args[0])
			return nil
		},
	}
}

func defaultImageName(file string) string {
	base := strings.ToLower(filepath.Base(file))
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

func formatFromExt(file string) string {
	switch ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(file), ".")); ext {
	case "qcow2", "raw", "ova", "vmdk", "vhd", "vhdx":
		return ext
	}
	return ""
}

func imageSpecSnippet(img uploadedImage) string {
	var b strings.Builder
	b.WriteString("image:\n  source:\n")
	fmt.Fprintf(&b, "    httpURL: %s\n", img.URL)
	if img.Format != "" {
		fmt.Fprintf(&b, "    format: %s\n", img.Format)
	}
	fmt.Fprintf(&b, "  digest: %s\n", img.Digest)
	return b.String()
}

func uiBase() (string, error) {
	base := uiBaseURL()
	if base == "" {
		return "", fmt.Errorf("set KAIRON_UI_URL to reach kairon-ui's image store")
	}
	return base, nil
}

func uploadImage(ctx context.Context, file, name, format string, replace bool) (uploadedImage, error) {
	var img uploadedImage
	base, err := uiBase()
	if err != nil {
		return img, err
	}
	f, err := os.Open(file)
	if err != nil {
		return img, err
	}
	defer func() { _ = f.Close() }()
	hasher := sha256.New()
	size, err := io.Copy(hasher, f)
	if err != nil {
		return img, fmt.Errorf("hash %s: %w", file, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return img, err
	}
	q := url.Values{}
	if format != "" {
		q.Set("format", format)
	}
	if replace {
		q.Set("replace", "true")
	}
	u := base + "/api/v1/images/" + url.PathEscape(name)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, f)
	if err != nil {
		return img, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Image-Digest", "sha256:"+hex.EncodeToString(hasher.Sum(nil)))
	if token := uiToken(false); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := uiDo(req)
	if err != nil {
		return img, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return img, uiError(resp)
	}
	return img, json.NewDecoder(resp.Body).Decode(&img)
}

func uiJSON(ctx context.Context, method, path string, out any) error {
	if _, err := uiBase(); err != nil {
		return err
	}
	resp, err := uiRequest(ctx, method, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return uiError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func uiError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(msg, &e) == nil && e.Error != "" {
		return fmt.Errorf("kairon-ui: %s: %s", resp.Status, e.Error)
	}
	return fmt.Errorf("kairon-ui: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
}
