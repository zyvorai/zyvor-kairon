// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package diskexport holds the disks kairon-node has agreed to serve for
// MachineExport objects. The agent fills it; the node relay serves only
// paths found here, never a path supplied by a request or by object status.
package diskexport

import (
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type Entry struct {
	Path    string
	Format  string
	Expires time.Time
	// Temp marks a converted copy the agent must remove when the entry goes.
	Temp bool
}

type Registry struct {
	mu sync.Mutex
	m  map[string]Entry
}

func Key(namespace, name string) string { return namespace + "/" + name }

func (r *Registry) Put(key string, e Entry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]Entry{}
	}
	r.m[key] = e
}

func (r *Registry) Get(key string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[key]
	return e, ok
}

// Delete removes key and returns what was there.
func (r *Registry) Delete(key string) (Entry, bool) {
	if r == nil {
		return Entry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[key]
	delete(r.m, key)
	return e, ok
}

func (r *Registry) Keys() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Serve streams the disk registered under key, with Range support so an
// interrupted download can resume.
func (r *Registry) Serve(w http.ResponseWriter, req *http.Request, key string) {
	e, ok := r.Get(key)
	if !ok || time.Now().After(e.Expires) {
		http.Error(w, "export not ready on this node", http.StatusNotFound)
		return
	}
	f, err := os.Open(e.Path)
	if err != nil {
		http.Error(w, "open export: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	// Block devices report size 0 from Stat; seeking to the end works for both.
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		http.Error(w, "size export: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "seek export: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Kairon-Disk-Format", e.Format)
	http.ServeContent(w, req, "", time.Time{}, io.NewSectionReader(f, 0, size))
}
