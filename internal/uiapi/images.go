// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// UploadedImage is one named entry in kairon-ui's image store. The bytes
// live content-addressed under <ImageStoreDir>/sha256/<hex>; names are
// pointers kept under <ImageStoreDir>/names/<name>.json.
type UploadedImage struct {
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	Format    string `json:"format,omitempty"`
	URL       string `json:"url"`
	// InsecureSkipTLSVerify is set when URL is https and served with a
	// self-signed certificate, so spec.image.source needs it too.
	InsecureSkipTLSVerify bool      `json:"insecureSkipTLSVerify,omitempty"`
	UploadedAt            time.Time `json:"uploadedAt"`
	UploadedBy            string    `json:"uploadedBy,omitempty"`
}

var imageNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`)

func (s *Server) imageStoreEnabled(w http.ResponseWriter) bool {
	if s.ImageStoreDir == "" {
		writeError(w, http.StatusNotImplemented, "the image store is not enabled on this deployment (ui.imageStore.enabled)")
		return false
	}
	return true
}

// imageURL is where kairon-node downloads a blob from: the configured
// public base (reachable from nodes) or, failing that, this request's host.
func (s *Server) imageURL(r *http.Request, hexDigest string) string {
	base := strings.TrimRight(s.ImageStorePublicURL, "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/images/sha256/" + hexDigest
}

func (s *Server) readImageEntry(name string) (UploadedImage, error) {
	var img UploadedImage
	b, err := os.ReadFile(filepath.Join(s.ImageStoreDir, "names", name+".json"))
	if err != nil {
		return img, err
	}
	return img, json.Unmarshal(b, &img)
}

// handleUploadImage streams the request body into the store, hashing as
// it goes, and points name at the result. Admin-only, like the node image
// catalog. Re-uploading identical bytes under a new name stores nothing.
func (s *Server) handleUploadImage(w http.ResponseWriter, r *http.Request) {
	if !s.imageStoreEnabled(w) {
		return
	}
	username := usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		writeError(w, http.StatusForbidden, "uploading images requires an admin account")
		return
	}
	name := r.PathValue("name")
	if !imageNameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "image name must be 1-128 lowercase letters, digits, '.', '_' or '-', starting and ending alphanumeric")
		return
	}
	format := r.URL.Query().Get("format")
	if !slices.Contains(model.ImageSourceFormats, format) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("format %q must be one of qcow2, raw, ova, vmdk, vhd, vhdx", format))
		return
	}
	if _, err := s.readImageEntry(name); err == nil && r.URL.Query().Get("replace") != "true" {
		writeError(w, http.StatusConflict, fmt.Sprintf("image %q already exists (pass replace=true to repoint it)", name))
		return
	}
	blobDir := filepath.Join(s.ImageStoreDir, "sha256")
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.ImageStoreMaxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.ImageStoreMaxBytes)
	}
	tmp, err := os.CreateTemp(blobDir, ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	hasher := sha256.New()
	size, err := io.Copy(tmp, io.TeeReader(r.Body, hasher))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("image exceeds the %d-byte upload limit", tooBig.Limit))
			return
		}
		writeError(w, http.StatusBadRequest, "upload interrupted: "+err.Error())
		return
	}
	if size == 0 {
		writeError(w, http.StatusBadRequest, "empty upload")
		return
	}
	hexDigest := hex.EncodeToString(hasher.Sum(nil))
	if want := r.Header.Get("X-Image-Digest"); want != "" && want != model.ImageDigestPrefix+hexDigest {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("digest mismatch: received %s%s, client sent %s", model.ImageDigestPrefix, hexDigest, want))
		return
	}
	blob := filepath.Join(blobDir, hexDigest)
	if _, err := os.Stat(blob); os.IsNotExist(err) {
		if err := os.Rename(tmp.Name(), blob); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	img := UploadedImage{
		Name: name, Digest: model.ImageDigestPrefix + hexDigest, SizeBytes: size, Format: format,
		URL: s.imageURL(r, hexDigest), UploadedAt: time.Now().UTC(), UploadedBy: username,
	}
	img.InsecureSkipTLSVerify = s.TLSSelfSigned && strings.HasPrefix(img.URL, "https://")
	if err := s.writeImageEntry(img); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, img)
}

func (s *Server) writeImageEntry(img UploadedImage) error {
	dir := filepath.Join(s.ImageStoreDir, "names")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(img)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+img.Name+".tmp")
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, img.Name+".json"))
}

func (s *Server) listImageEntries() ([]UploadedImage, error) {
	entries, err := os.ReadDir(filepath.Join(s.ImageStoreDir, "names"))
	if os.IsNotExist(err) {
		return []UploadedImage{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []UploadedImage{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(name, ".") {
			continue
		}
		if img, err := s.readImageEntry(name); err == nil {
			out = append(out, img)
		}
	}
	return out, nil
}

// handleListImages lists the store. Any authenticated operator.
func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	if !s.imageStoreEnabled(w) {
		return
	}
	imgs, err := s.listImageEntries()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, imgs)
}

// handleDeleteImage drops a name and, once no other name points at it,
// the blob. Nodes keep their own cached copies either way.
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	if !s.imageStoreEnabled(w) {
		return
	}
	if !s.isAdminIdentity(r.Context(), usernameFromContext(r.Context())) {
		writeError(w, http.StatusForbidden, "deleting images requires an admin account")
		return
	}
	name := r.PathValue("name")
	if !imageNameRE.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid image name")
		return
	}
	img, err := s.readImageEntry(name)
	if os.IsNotExist(err) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("image %q not found", name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Remove(filepath.Join(s.ImageStoreDir, "names", name+".json")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest, err := s.listImageEntries()
	if err == nil && !slices.ContainsFunc(rest, func(o UploadedImage) bool { return o.Digest == img.Digest }) {
		_ = os.Remove(filepath.Join(s.ImageStoreDir, "sha256", strings.TrimPrefix(img.Digest, model.ImageDigestPrefix)))
	}
	w.WriteHeader(http.StatusNoContent)
}

var hexDigestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// handleServeImageBlob serves a blob by digest without authentication:
// kairon-node has no kairon-ui credentials, and it verifies the bytes
// against spec.image.digest anyway. Anyone who can reach kairon-ui and
// knows a digest can download that image.
func (s *Server) handleServeImageBlob(w http.ResponseWriter, r *http.Request) {
	if !s.imageStoreEnabled(w) {
		return
	}
	hexDigest := r.PathValue("digest")
	if !hexDigestRE.MatchString(hexDigest) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.ImageStoreDir, "sha256", hexDigest))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
