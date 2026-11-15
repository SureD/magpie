package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/usage"
)

func TestObserveCaptureKeepsToolsAndDropsHeaderSecrets(t *testing.T) {
	fresh(t)
	serveOn(t, "fake", "k", []string{"m1"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`)
	}))
	if err := sessions.SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	const headerSecret = "super-secret-token-value"
	body := `{"model":"fake/m1","messages":[{"role":"user","content":"look"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"notes.txt\",\"api_key\":\"sk-test-secret-value-123456\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"file says hello-synthetic"}]}`
	s := New()
	post := func(session, project, content string) *httptest.ResponseRecorder {
		t.Helper()
		payload := body
		if content != "" {
			payload = `{"model":"fake/m1","messages":[{"role":"user","content":"` + content + `"}]}`
		}
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+headerSecret)
		r.Header.Set("Cookie", "sid=cookie-secret-value")
		r.Header.Set("User-Agent", "review-client/1")
		if session != "" {
			r.Header.Set(SessionHeader, session)
		}
		if project != "" {
			r.Header.Set(ProjectHeader, project)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("relay %d %s", w.Code, w.Body)
		}
		return w
	}
	post("sess-tools", "synthetic-project", "")
	page, err := sessions.GatewayTurnPage("review-client", "sess-tools", time.Time{}, "", 10)
	if err != nil || len(page.Turns) != 1 {
		t.Fatalf("turn %+v %v", page, err)
	}
	turn := page.Turns[0]
	raw, _ := json.Marshal(turn)
	if strings.Contains(string(raw), headerSecret) || strings.Contains(string(raw), "cookie-secret-value") || strings.Contains(string(raw), "sk-test-secret-value-123456") {
		t.Fatalf("secret stored in the turn: %s", raw)
	}
	if turn.Project != "synthetic-project" {
		t.Fatalf("project %q", turn.Project)
	}
	if !strings.Contains(string(raw), "hello-synthetic") || !strings.Contains(string(raw), "notes.txt") {
		t.Fatalf("tool text missing: %s", raw)
	}
	if turn.Usage == nil || turn.Usage.Input != 11 || turn.Usage.Output != 2 {
		t.Fatalf("usage snapshot %+v", turn.Usage)
	}
	if turn.Usage.CostKnown || turn.Usage.ContextKnown {
		t.Fatalf("invented cost or context: %+v", turn.Usage)
	}
	post("sess-other", "", "other-session-only")
	other, err := sessions.GatewayTurnPage("review-client", "sess-other", time.Time{}, "", 10)
	if err != nil || len(other.Turns) != 1 || strings.Contains(string(mustJSON(other.Turns[0])), "hello-synthetic") {
		t.Fatalf("sessions mixed: %+v %v", other, err)
	}
	post("", "", "nameless-synthetic")
	listed, err := sessions.ListGatewaySessions(sessions.SessionListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range listed.Sessions {
		if strings.Contains(info.Title, "nameless") {
			t.Fatalf("nameless call became a session: %+v", info)
		}
	}
}

func TestObserveCaptureMarksAnUnfinishedStream(t *testing.T) {
	fresh(t)
	serveOn(t, "fake", "k", []string{"m1"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial-synthetic\"}}]}\n\n")
	}))
	if err := sessions.SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fake/m1","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	r.Header.Set("User-Agent", "review-client/1")
	r.Header.Set(SessionHeader, "sess-cut")
	w := httptest.NewRecorder()
	New().Handler().ServeHTTP(w, r)
	page, err := sessions.GatewayTurnPage("review-client", "sess-cut", time.Time{}, "", 5)
	if err != nil || len(page.Turns) != 1 {
		t.Fatalf("%+v %v %s", page, err, w.Body)
	}
	turn := page.Turns[0]
	raw, _ := json.Marshal(turn)
	if !strings.Contains(string(raw), "partial-synthetic") {
		t.Fatalf("partial text missing: %s", raw)
	}
	if !turn.Incomplete && !turn.Cut {
		t.Fatalf("unfinished stream stored as complete: %+v", turn)
	}
}

func TestRecordedCancelIsMarkedAndNotPriced(t *testing.T) {
	var turn sessions.GatewayTurn
	applyRecordedUsage(&turn, usage.Record{Status: usage.StatusCanceled, ErrType: usage.CanceledErrType, Input: 4, Output: 0, Provider: "fake", Model: "m1", RequestID: "rid-cancel"})
	if !turn.Cancelled || !turn.Incomplete || turn.RequestID != "rid-cancel" {
		t.Fatalf("%+v", turn)
	}
	if turn.Usage == nil || turn.Usage.CostKnown || turn.Usage.ContextKnown || turn.Usage.Input != 4 {
		t.Fatalf("usage %+v", turn.Usage)
	}
}
