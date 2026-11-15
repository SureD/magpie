package observe

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/testenv"
	"github.com/yetone/magpie/internal/usage"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func TestObserveInsideTheAppNeedsNoKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mux := http.NewServeMux()
	Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/observe/bootstrap", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "confirm_phrase") {
		t.Fatalf("in-app bootstrap %d %s", rec.Code, rec.Body)
	}
	page := httptest.NewRecorder()
	mux.ServeHTTP(page, httptest.NewRequest("GET", "/observe/?embed=1&api=/api/observe", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Magpie") {
		t.Fatalf("in-app page %d %s", page.Code, page.Body)
	}
}

func TestObserveAPIRequiresKeyAndPaginates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := sessions.SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	for i := range 3 {
		err := sessions.SaveGatewayTurn(sessions.GatewayTurn{
			Agent: "app", Session: "s", Time: at.Add(time.Duration(i) * time.Second),
			Input:  []sessions.Part{{Role: "user", Kind: "text", Text: "synthetic turn"}},
			Output: []sessions.Part{{Role: "assistant", Kind: "text", Text: "ok"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	usage.Append(usage.Record{Time: at, Agent: "app", Provider: "fake", Model: "m1", Session: "s", Input: 10, Output: 4, CacheRead: 2, CacheWrite: 1, Status: 200, Millis: 15})
	h := Handler("test-key-0123456789")
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest("GET", "/api/sessions", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("auth %d", unauth.Code)
	}
	res := get(t, h, "/api/sessions?agent=app")
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"id":"s"`) {
		t.Fatalf("list %d %s", res.Code, res.Body)
	}
	turns := get(t, h, "/api/turns?agent=app&id=s&limit=2")
	var page sessions.TurnPage
	if err := json.Unmarshal(turns.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 2 || !page.HasMore {
		t.Fatalf("page %+v", page)
	}
	series := get(t, h, "/api/series?agent=app&id=s")
	if !strings.Contains(series.Body.String(), "billed_input") || !strings.Contains(series.Body.String(), "input_context") {
		t.Fatalf("series %s", series.Body)
	}
	if strings.Contains(series.Body.String(), `"context_known":true`) {
		t.Fatalf("context invented: %s", series.Body)
	}
	on := post(t, h, "/api/recording", `{"on":true}`)
	if on.Code != http.StatusBadRequest {
		t.Fatalf("recording without consent %d %s", on.Code, on.Body)
	}
	body := `{"on":true,"confirm":"` + confirmPhrase + `","days":0,"store_mb":0,"session_mb":0}`
	ok := post(t, h, "/api/recording", body)
	if ok.Code != 200 || !strings.Contains(ok.Body.String(), `"recording":true`) {
		t.Fatalf("enable %d %s", ok.Code, ok.Body)
	}
	add := post(t, h, "/api/providers", `{"name":"Desk Relay","protocol":"chat","base_url":"https://example.test/v1","key":"sk-synthetic","models":["demo-model"],"context":32000}`)
	if add.Code != 200 {
		t.Fatalf("provider %d %s", add.Code, add.Body)
	}
	listed := get(t, h, "/api/providers")
	if strings.Contains(listed.Body.String(), "sk-synthetic") {
		t.Fatalf("key returned: %s", listed.Body)
	}
	if !strings.Contains(listed.Body.String(), "desk-relay") || !strings.Contains(listed.Body.String(), "32000") {
		t.Fatalf("provider list %s", listed.Body)
	}
	var found bool
	for _, p := range provider.All() {
		if p.ID == "desk-relay" && p.Key == "sk-synthetic" && p.Chat == "https://example.test/v1" {
			found = true
		}
	}
	if !found {
		t.Fatal("provider was not saved")
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer test-key-0123456789")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
	r.Header.Set("Authorization", "Bearer test-key-0123456789")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
