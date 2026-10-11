// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func selftestServer(t *testing.T, fk *fakeKube) (http.Handler, string) {
	t.Helper()
	s := newTestServer(t, fk, "")
	s.Users = []User{
		{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true},
		{Username: "ops", PasswordHash: hashFor(t, "correct-horse"), Role: "operator"},
	}
	s.SessionSecret = []byte("test-session-secret")
	h := s.Handler()
	return h, loginToken(t, h, "root", "correct-horse")
}

func waitSelftest(t *testing.T, h http.Handler, token, id string) selftestView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rr := doJSON(t, h, http.MethodGet, "/api/v1/selftest/"+id, token, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("get run: %d %s", rr.Code, rr.Body.String())
		}
		var v selftestView
		if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if v.State != "running" {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("self-test run did not finish")
	return selftestView{}
}

func TestSelftestControlPlaneRunPassesWithCapableNode(t *testing.T) {
	fk := newFakeKube()
	fk.nodes = []model.Node{{Metadata: model.ObjectMeta{Name: "w1", Labels: map[string]string{model.CapableLabel: "true"}}}}
	h, token := selftestServer(t, fk)

	rr := doJSON(t, h, http.MethodPost, "/api/v1/selftest", token, selftestRequest{Profile: "quick", NoVM: true})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	var started struct{ ID string }
	_ = json.Unmarshal(rr.Body.Bytes(), &started)

	v := waitSelftest(t, h, token, started.ID)
	if v.State != "passed" {
		t.Fatalf("state = %q; lines=%+v steps=%+v", v.State, v.Lines, v.Steps)
	}
	if v.Summary == nil || v.Summary.Pass != 1 {
		t.Errorf("summary = %+v", v.Summary)
	}
	var cp *selftestStep
	for i := range v.Steps {
		if v.Steps[i].ID == "control-plane" {
			cp = &v.Steps[i]
		}
	}
	if cp == nil || cp.Status != "pass" {
		t.Errorf("control-plane step = %+v", cp)
	}
	if len(v.Lines) == 0 {
		t.Error("expected log lines")
	}

	// ?after= returns only newer lines.
	rr = doJSON(t, h, http.MethodGet, "/api/v1/selftest/"+started.ID+"?after="+itoa(v.Next), token, nil)
	var tail selftestView
	_ = json.Unmarshal(rr.Body.Bytes(), &tail)
	if len(tail.Lines) != 0 {
		t.Errorf("after=%d should return no lines, got %d", v.Next, len(tail.Lines))
	}
}

func TestSelftestFailsWithoutCapableNode(t *testing.T) {
	fk := newFakeKube()
	fk.nodes = []model.Node{{Metadata: model.ObjectMeta{Name: "w1"}}}
	h, token := selftestServer(t, fk)

	rr := doJSON(t, h, http.MethodPost, "/api/v1/selftest", token, selftestRequest{Profile: "quick", NoVM: true})
	var started struct{ ID string }
	_ = json.Unmarshal(rr.Body.Bytes(), &started)
	v := waitSelftest(t, h, token, started.ID)
	if v.State != "failed" {
		t.Errorf("state = %q, want failed", v.State)
	}
}

func TestSelftestRequiresAdminAndRejectsBadInput(t *testing.T) {
	fk := newFakeKube()
	h, adminToken := selftestServer(t, fk)
	opsToken := loginToken(t, h, "ops", "correct-horse")

	for _, tc := range []struct {
		name, method, path, token string
		body                      any
		want                      int
	}{
		{"operator cannot start", http.MethodPost, "/api/v1/selftest", opsToken, selftestRequest{Profile: "quick"}, http.StatusForbidden},
		{"operator cannot list", http.MethodGet, "/api/v1/selftest", opsToken, nil, http.StatusForbidden},
		{"bad profile", http.MethodPost, "/api/v1/selftest", adminToken, selftestRequest{Profile: "huge"}, http.StatusBadRequest},
		{"unknown scenario", http.MethodPost, "/api/v1/selftest", adminToken, selftestRequest{Only: []string{"nope"}}, http.StatusBadRequest},
		{"unknown run", http.MethodGet, "/api/v1/selftest/deadbeef", adminToken, nil, http.StatusNotFound},
		{"cancel unknown run", http.MethodDelete, "/api/v1/selftest/deadbeef", adminToken, nil, http.StatusNotFound},
		{"catalogue is readable", http.MethodGet, "/api/v1/selftest/scenarios", opsToken, nil, http.StatusOK},
	} {
		rr := doJSON(t, h, tc.method, tc.path, tc.token, tc.body)
		if rr.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rr.Code, tc.want, rr.Body.String())
		}
	}
}

// A finished run must not block the next one; the "one at a time" refusal
// itself is covered by TestSelftestStoreRejectsConcurrentRun.
func TestSelftestFinishedRunDoesNotBlockNext(t *testing.T) {
	fk := newFakeKube()
	h, token := selftestServer(t, fk)
	first := doJSON(t, h, http.MethodPost, "/api/v1/selftest", token, selftestRequest{Profile: "quick", NoVM: true})
	if first.Code != http.StatusAccepted {
		t.Fatalf("first start: %d", first.Code)
	}
	var f struct{ ID string }
	_ = json.Unmarshal(first.Body.Bytes(), &f)
	waitSelftest(t, h, token, f.ID)
	second := doJSON(t, h, http.MethodPost, "/api/v1/selftest", token, selftestRequest{Profile: "quick", NoVM: true})
	if second.Code != http.StatusAccepted {
		t.Errorf("a finished run must not block the next one: %d %s", second.Code, second.Body.String())
	}
}

func TestSelftestStoreRejectsConcurrentRun(t *testing.T) {
	s := &Server{}
	st := s.selftests()
	st.jobs = append(st.jobs, &selftestJob{ID: "r1", State: "running"})
	if st.running() == nil {
		t.Fatal("a running job must be reported as running")
	}
	st.jobs[0].State = "passed"
	if st.running() != nil {
		t.Fatal("a finished job must not block")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
