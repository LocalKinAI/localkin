package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAskPermission_AnsweredByPost(t *testing.T) {
	srv := New("127.0.0.1:0", nil, func(context.Context, string) {})
	sub := srv.subscribe()
	defer srv.unsubscribe(sub)

	type res struct {
		d   string
		err error
	}
	done := make(chan res, 1)
	go func() {
		d, err := srv.AskPermission(context.Background(), Event{ID: "perm-1", Name: "shell", Summary: "shell: ls"})
		done <- res{d, err}
	}()

	// The request went out on the stream.
	select {
	case ev := <-sub:
		if ev.Type != "permission_request" || ev.ID != "perm-1" || ev.Summary != "shell: ls" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no permission_request event")
	}
	if p := srv.PendingPermissions(); len(p) != 1 || p[0] != "perm-1" {
		t.Fatalf("pending: %v", p)
	}

	// Wrong id → 404, right id → 202 and the waiter wakes.
	rr := httptest.NewRecorder()
	srv.handlePermission(rr, httptest.NewRequest(http.MethodPost, "/api/permission", strings.NewReader(`{"id":"nope","decision":"allow"}`)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("stale id should 404, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	srv.handlePermission(rr, httptest.NewRequest(http.MethodPost, "/api/permission", strings.NewReader(`{"id":"perm-1","decision":"allow_always"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
	select {
	case r := <-done:
		if r.err != nil || r.d != "allow_always" {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never woke")
	}
}

func TestAskPermission_InterruptedTurnDenies(t *testing.T) {
	srv := New("127.0.0.1:0", nil, func(context.Context, string) {})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	d, err := srv.AskPermission(ctx, Event{ID: "perm-2"})
	if d != "deny" || err == nil {
		t.Fatalf("got %q %v", d, err)
	}
	if len(srv.PendingPermissions()) != 0 {
		t.Fatal("waiter should be cleaned up")
	}
}

func TestHandlePermission_RejectsBadDecision(t *testing.T) {
	srv := New("127.0.0.1:0", nil, func(context.Context, string) {})
	rr := httptest.NewRecorder()
	srv.handlePermission(rr, httptest.NewRequest(http.MethodPost, "/api/permission", strings.NewReader(`{"id":"x","decision":"maybe"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestHandlePlanMode_RoundTripAndBroadcast(t *testing.T) {
	srv := New("127.0.0.1:0", nil, func(context.Context, string) {})
	var state bool
	srv.SetPlanModeHandler(func(on bool) bool { state = on; return on })
	sub := srv.subscribe()
	defer srv.unsubscribe(sub)
	rr := httptest.NewRecorder()
	srv.handlePlanMode(rr, httptest.NewRequest(http.MethodPost, "/api/plan_mode", strings.NewReader(`{"enabled":true}`)))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"enabled":true`) || !state {
		t.Fatalf("code=%d body=%s state=%v", rr.Code, rr.Body.String(), state)
	}
	select {
	case ev := <-sub:
		if ev.Type != "plan_mode" || !ev.PlanMode {
			t.Fatalf("unexpected broadcast: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no plan_mode broadcast")
	}
}

func TestAskQuestion_AnsweredByPost(t *testing.T) {
	srv := New("127.0.0.1:0", nil, func(context.Context, string) {})
	sub := srv.subscribe()
	defer srv.unsubscribe(sub)
	done := make(chan string, 1)
	go func() {
		a, _ := srv.AskQuestion(context.Background(), Event{ID: "q-1", Message: "Which folder?", Options: []string{"A", "B"}})
		done <- a
	}()
	select {
	case ev := <-sub:
		if ev.Type != "question" || len(ev.Options) != 2 || ev.Message != "Which folder?" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no question event")
	}
	rr := httptest.NewRecorder()
	srv.handleAnswer(rr, httptest.NewRequest(http.MethodPost, "/api/answer", strings.NewReader(`{"id":"q-1","text":"B"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
	select {
	case a := <-done:
		if a != "B" {
			t.Fatalf("answer %q", a)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never woke")
	}
}

func TestHandleWorkspace(t *testing.T) {
	srv := New("127.0.0.1:0", nil, func(context.Context, string) {})
	current := "/tmp"
	srv.SetStateHandler(func() State { return State{Workspace: current} })
	srv.SetWorkspaceHandler(func(p string) (string, error) {
		if p == "/nope" {
			return "", fmt.Errorf("no such dir")
		}
		current = p
		return p, nil
	})
	rr := httptest.NewRecorder()
	srv.handleWorkspace(rr, httptest.NewRequest(http.MethodGet, "/api/workspace", nil))
	if !strings.Contains(rr.Body.String(), `"/tmp"`) {
		t.Fatalf("GET: %s", rr.Body.String())
	}
	rr = httptest.NewRecorder()
	srv.handleWorkspace(rr, httptest.NewRequest(http.MethodPost, "/api/workspace", strings.NewReader(`{"path":"/nope"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad path should 400, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	srv.handleWorkspace(rr, httptest.NewRequest(http.MethodPost, "/api/workspace", strings.NewReader(`{"path":"/Users"}`)))
	if rr.Code != http.StatusOK || current != "/Users" || srv.FileURL("/Users/x.png") == "" {
		t.Fatalf("POST: %d %s url=%q", rr.Code, rr.Body.String(), srv.FileURL("/Users/x.png"))
	}
}
