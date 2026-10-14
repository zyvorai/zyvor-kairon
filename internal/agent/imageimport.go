// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// resolveImageSource downloads m.Spec.Image.Source into a.ImageCacheDir if
// not already cached under its digest, verifies it against
// m.Spec.Image.Digest (required -- see model.ValidateImageSource), and returns
// the cached file's absolute path. Idempotent: a second Machine (or a
// later reconcile tick of the same Machine) naming the same digest never
// re-downloads, it just stats the existing cache file -- this is the
// whole answer to "50 Machines booting the same golden image" without a
// separate cache/refcount data structure, since the cache path is itself
// content-addressed and immutable once written.
func (a *Agent) resolveImageSource(ctx context.Context, m model.Machine) (string, error) {
	if a.ImageCacheDir == "" {
		return "", fmt.Errorf("this node has no image cache configured (-image-cache-dir / $KAIRON_IMAGE_CACHE_DIR) -- spec.image.source cannot be used without it; see docs/guides/machine-image-import.md")
	}
	if err := model.ValidateImageSource(m.Spec.Image); err != nil {
		return "", err
	}
	return a.fetchToCache(ctx, m.Spec.Image)
}

// fetchToCache returns the digest-keyed cache path for img.Source,
// downloading (or pulling) it first when absent. img must already be
// validated.
func (a *Agent) fetchToCache(ctx context.Context, img model.ImageSpec) (string, error) {
	hexDigest := strings.TrimPrefix(img.Digest, model.ImageDigestPrefix)
	cachePath := filepath.Join(a.ImageCacheDir, "sha256", hexDigest)
	if img.Source.OCI != "" {
		// Keyed by manifest digest, not disk bytes, so kept apart.
		cachePath = filepath.Join(a.ImageCacheDir, "oci", "sha256", hexDigest)
	}
	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat image cache entry %s: %w", cachePath, err)
	}
	if img.Source.OCI != "" {
		if err := a.pullOCIDisk(ctx, img, cachePath); err != nil {
			return "", err
		}
		return cachePath, nil
	}
	if err := downloadToTemp(ctx, img.Source.HTTPURL, cachePath, hexDigest, img.Source.InsecureSkipTLSVerify); err != nil {
		return "", err
	}
	return cachePath, nil
}

// resolveCdroms fills each spec.cdroms entry's Path from the image cache.
// Entries still waiting on an imageRef never reach a node (the controller
// holds the Machine back), so a missing source here is an error.
func (a *Agent) resolveCdroms(ctx context.Context, m *model.Machine) error {
	if len(m.Spec.Cdroms) == 0 {
		return nil
	}
	if a.ImageCacheDir == "" {
		return fmt.Errorf("this node has no image cache configured (-image-cache-dir / $KAIRON_IMAGE_CACHE_DIR) -- spec.cdroms cannot be used without it")
	}
	if err := model.ValidateCdroms(m.Spec.Cdroms); err != nil {
		return err
	}
	for i := range m.Spec.Cdroms {
		cd := &m.Spec.Cdroms[i]
		if cd.Source == nil {
			return fmt.Errorf("spec.cdroms[%d]: imageRef %q is not resolved yet", i, cd.ImageRef)
		}
		path, err := a.fetchToCache(ctx, model.ImageSpec{Source: cd.Source, Digest: cd.Digest})
		if err != nil {
			return fmt.Errorf("spec.cdroms[%d]: %w", i, err)
		}
		cd.Path = path
	}
	return nil
}

// blankBaseSize is the empty raw file every blank root disk overlays;
// FluxVM grows the overlay to spec.image.diskSize.
const blankBaseSize = 1 << 20

// blankBase returns the shared empty base image, creating it once.
func (a *Agent) blankBase() (string, error) {
	if a.ImageCacheDir == "" {
		return "", fmt.Errorf("this node has no image cache configured (-image-cache-dir / $KAIRON_IMAGE_CACHE_DIR) -- spec.image.blank cannot be used without it")
	}
	path := filepath.Join(a.ImageCacheDir, "blank", "empty.raw")
	if fi, err := os.Stat(path); err == nil && fi.Size() == blankBaseSize {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("create blank image directory: %w", err)
	}
	if err := writeSparseFile(path, blankBaseSize); err != nil {
		return "", fmt.Errorf("create blank base image: %w", err)
	}
	return path, nil
}

// writeSparseFile atomically creates path as a zero-filled file of size bytes.
func writeSparseFile(path string, size int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".blank-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Truncate(size); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// populateBootVolume seeds an empty spec.volumes[0] boot disk: with the
// cached image at sourcePath, or (sourcePath empty) a blank raw disk of
// sizeGiB. An existing non-empty disk is never touched, so this only ever
// runs before a Machine's first boot. A Block-mode volume gets the image
// written onto the device (see seedBlockDevice).
func populateBootVolume(bootDisk, sourcePath string, sizeGiB uint64, raw bool) error {
	fi, err := os.Stat(bootDisk)
	switch {
	case err == nil && fi.Mode()&os.ModeDevice != 0:
		return seedBlockDevice(bootDisk, sourcePath)
	case err == nil && fi.Size() > 0:
		return nil
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("stat boot disk %s: %w", bootDisk, err)
	}
	if sourcePath == "" {
		if sizeGiB == 0 {
			return fmt.Errorf("spec.image.blank needs spec.image.diskSize")
		}
		return writeSparseFile(bootDisk, int64(sizeGiB)<<30)
	}
	if raw {
		return convertRawAtomic(sourcePath, bootDisk)
	}
	return copyFileAtomic(sourcePath, bootDisk)
}

// convertRawAtomic is copyFileAtomic for a disk FluxVM opens as raw: a
// qcow2 source is converted with qemu-img on the way.
func convertRawAtomic(src, dst string) error {
	magic := make([]byte, len(qcow2Magic))
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	_, err = io.ReadFull(f, magic)
	_ = f.Close()
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("read image %s: %w", src, err)
	}
	if !bytes.Equal(magic, qcow2Magic) {
		return copyFileAtomic(src, dst)
	}
	if _, err := exec.LookPath(qemuImg); err != nil {
		return fmt.Errorf("a shared-storage boot disk is raw; converting qcow2 image %s needs qemu-img on kairon-node's PATH, or use a raw image", src)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".seed-*")
	if err != nil {
		return fmt.Errorf("seed boot volume: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()
	if out, err := exec.Command(qemuImg, "convert", "-O", "raw", src, tmpPath).CombinedOutput(); err != nil {
		return fmt.Errorf("convert image %s to raw: %w: %s", src, err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmpPath, dst)
}

// qemuImg is the qemu-img binary seedBlockDevice runs for a qcow2 image; a
// var for tests.
var qemuImg = "qemu-img"

// qcow2Magic starts every qcow2 image.
var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

// seedBlockDevice writes the image at sourcePath onto device as raw, but
// only while the device's first MiB is still all zeros -- a device that
// already holds anything (an earlier seed, a guest's own writes) is never
// overwritten. A blank Machine (no sourcePath) needs nothing written. A
// raw image is copied as is; a qcow2 one needs qemu-img, which the
// distroless kairon-node image does not have.
func seedBlockDevice(device, sourcePath string) error {
	if sourcePath == "" {
		return nil
	}
	f, err := os.Open(device)
	if err != nil {
		return fmt.Errorf("open boot device %s: %w", device, err)
	}
	head := make([]byte, 1<<20)
	n, err := io.ReadFull(f, head)
	_ = f.Close()
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("read boot device %s: %w", device, err)
	}
	for _, b := range head[:n] {
		if b != 0 {
			return nil
		}
	}
	src, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	magic := make([]byte, len(qcow2Magic))
	if _, err := io.ReadFull(src, magic); err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("read image %s: %w", sourcePath, err)
	}
	if !bytes.Equal(magic, qcow2Magic) {
		return copyOntoDevice(src, device)
	}
	if _, err := exec.LookPath(qemuImg); err != nil {
		return fmt.Errorf("writing a qcow2 image onto Block volume %s needs qemu-img on kairon-node's PATH, which the kairon-node container image does not have; use a raw image (spec.image.source.format: raw)", device)
	}
	out, err := exec.Command(qemuImg, "convert", "-n", "-O", "raw", sourcePath, device).CombinedOutput()
	if err != nil {
		return fmt.Errorf("write image onto boot device %s: %w: %s", device, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// copyOntoDevice writes all of src onto device from offset 0 and syncs.
func copyOntoDevice(src *os.File, device string) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dst, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open boot device %s: %w", device, err)
	}
	if _, err := io.CopyBuffer(dst, src, make([]byte, 1<<20)); err != nil {
		_ = dst.Close()
		return fmt.Errorf("write image onto boot device %s: %w", device, err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("sync boot device %s: %w", device, err)
	}
	return dst.Close()
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".seed-*")
	if err != nil {
		return fmt.Errorf("seed boot volume: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("seed boot volume: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, dst)
}

// downloadToTemp streams source into a temp file in the same directory as
// destPath (so the final os.Rename is same-filesystem and atomic -- no
// partial file is ever visible at destPath), hashing as it streams, and
// verifies the result against wantHexDigest before the rename. No format
// conversion: FluxVM already consumes qcow2/raw directly (see
// docs/guides/machine-storage.md), so bytes are trusted via digest, not
// inspected -- which is also why skipping TLS verification is safe for
// integrity.
func downloadToTemp(ctx context.Context, source, destPath, wantHexDigest string, insecure bool) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("create image cache directory: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", source, err)
	}
	client := http.DefaultClient
	if insecure {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // bytes are verified against the pinned sha256 below
		client = &http.Client{Transport: tr}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", source, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected status %s", source, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".import-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", source, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename below succeeds

	hasher := sha256.New()
	if _, err := io.Copy(tmp, io.TeeReader(resp.Body, hasher)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("download %s: %w", source, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("finalize download of %s: %w", source, err)
	}

	gotHexDigest := hex.EncodeToString(hasher.Sum(nil))
	if gotHexDigest != wantHexDigest {
		return fmt.Errorf("downloaded %s: digest mismatch, got sha256:%s, want sha256:%s", source, gotHexDigest, wantHexDigest)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("finalize image cache entry for %s: %w", source, err)
	}
	return nil
}

// importedImage is the record kairon-node keeps next to the download cache
// for a source that went through FluxVM's import, so later reconciles and
// other Machines with the same digest reuse the converted disk.
type importedImage struct {
	Image      string   `json:"image"`
	ExtraDisks []string `json:"extraDisks,omitempty"`
	Actions    []string `json:"actions,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// dataDisks names the import's extra disks model.ImportDiskName(1..N) in
// source order.
func (r importedImage) dataDisks() []fluxvm.DataDisk {
	out := make([]fluxvm.DataDisk, 0, len(r.ExtraDisks))
	for i, p := range r.ExtraDisks {
		out = append(out, fluxvm.DataDisk{Name: model.ImportDiskName(i + 1), Backing: p})
	}
	return out
}

// importName is the FluxVM import name for a digest and repair choice; it
// is also the record's file name under <cache>/imported/.
func importName(img model.ImageSpec) string {
	hexDigest := strings.TrimPrefix(img.Digest, model.ImageDigestPrefix)
	name := "kairon-" + hexDigest[:24]
	if img.Source.Repair {
		name += "-repaired"
	}
	return name
}

// resolveImportedImage converts (and with Repair, fixes) the downloaded
// file at cachedPath through FluxVM's POST /v1/images/import and returns
// the boot disk path. Content-addressed like the download cache: one
// import per digest and repair choice per node.
func (a *Agent) resolveImportedImage(ctx context.Context, m model.Machine, cachedPath string) (importedImage, error) {
	name := importName(m.Spec.Image)
	recordPath := filepath.Join(a.ImageCacheDir, "imported", name+".json")
	if b, err := os.ReadFile(recordPath); err == nil {
		var rec importedImage
		if err := json.Unmarshal(b, &rec); err == nil && rec.Image != "" {
			return rec, nil
		}
	}
	res, err := a.Flux.ImportImage(ctx, cachedPath, name, m.Spec.Image.Source.Repair)
	if err != nil {
		return importedImage{}, fmt.Errorf("import %s (%s): %w", m.Spec.Image.Source.Location(), dash(m.Spec.Image.Source.Format), err)
	}
	rec := importedImage{Image: res.Image, ExtraDisks: res.ExtraDisks}
	if res.Repair != nil {
		rec.Actions, rec.Warnings = res.Repair.Actions, res.Repair.Warnings
		a.Log.Info("image repaired", "machine", m.Metadata.Name, "os", res.Repair.Distro, "actions", len(res.Repair.Actions), "warnings", strings.Join(res.Repair.Warnings, "; "))
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil {
		return importedImage{}, err
	}
	b, _ := json.Marshal(rec)
	tmp := recordPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return importedImage{}, err
	}
	if err := os.Rename(tmp, recordPath); err != nil {
		return importedImage{}, err
	}
	return rec, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
