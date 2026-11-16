//go:build screenshot

package observe

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sessions"
)

func TestShot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := sessions.SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour)
	user := sessions.Part{Role: "user", Kind: "text", Text: "run pwd"}
	call := sessions.Part{Role: "assistant", Kind: "tool_use", Name: "bash", Text: `{"command":"pwd"}`}
	result := sessions.Part{Role: "tool", Kind: "tool_result", Name: "bash", Text: "/tmp"}
	done := sessions.Part{Role: "assistant", Kind: "text", Text: "it is /tmp"}
	ping := sessions.Part{Role: "user", Kind: "text", Text: "ping"}
	pong := sessions.Part{Role: "assistant", Kind: "text", Text: "pong"}
	steps := []sessions.GatewayTurn{
		{Input: []sessions.Part{user}, Output: []sessions.Part{call}},
		{Input: []sessions.Part{user, call, result}, Output: []sessions.Part{done}},
		{Input: []sessions.Part{user, call, result, done, ping}, Output: []sessions.Part{pong}},
		{Input: []sessions.Part{user, call, result, done, ping, pong, ping}, Output: []sessions.Part{{Role: "assistant", Kind: "text", Text: "pong again"}}},
		{Input: []sessions.Part{{Role: "user", Kind: "text", Text: "edited"}}, Output: []sessions.Part{{Role: "assistant", Kind: "text", Text: "fork"}}},
	}
	for i, turn := range steps {
		turn.Agent, turn.Session = "app", "synth"
		turn.Time = at.Add(time.Duration(i) * time.Second)
		if err := sessions.SaveGatewayTurn(turn); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(Handler(""))
	defer srv.Close()
	for _, theme := range []string{"light", "dark"} {
		out := "/tmp/synthetic-thread-" + theme + ".png"
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		cmd := exec.CommandContext(ctx, "google-chrome", "--headless=new", "--disable-gpu", "--no-sandbox", "--disable-dev-shm-usage", "--window-size=1100,900", "--virtual-time-budget=5000", "--screenshot="+out, srv.URL+"/?open=app/synth&theme="+theme)
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
		b, err := cmd.CombinedOutput()
		cancel()
		t.Logf("%s chrome: %v %s", theme, err, b)
		st, statErr := os.Stat(out)
		if statErr != nil || st.Size() < 1000 {
			t.Fatalf("%s screenshot: %v", theme, statErr)
		}
	}
}
