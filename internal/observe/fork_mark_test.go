package observe

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestForkMarkCSSIsShared(t *testing.T) {
	a, err := os.ReadFile("../gui/assets/fork.css")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("assets/fork.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("internal/gui/assets/fork.css and internal/observe/assets/fork.css differ")
	}
	for _, need := range []string{"--fork:", "--fork-ink:", ".fork-badge::before", "rotate(45deg)", ".fork-legend"} {
		if !bytes.Contains(a, []byte(need)) {
			t.Fatalf("fork.css missing %s", need)
		}
	}
}

func TestForkMarkOnlyOnCustomSurfaces(t *testing.T) {
	shell, err := os.ReadFile("../gui/assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(shell), "fork-badge") != 1 {
		t.Fatalf("the shell should mark only the Conversations entry, found %d badges", strings.Count(string(shell), "fork-badge"))
	}
	if !strings.Contains(string(shell), `data-view="observe" class="fork-nav"`) {
		t.Fatal("Conversations nav is not marked")
	}
	if strings.Contains(string(shell), `data-view="providers" class="fork-nav"`) || strings.Contains(string(shell), `data-view="sessions" class="fork-nav"`) {
		t.Fatal("an original nav entry was marked as custom")
	}
	page, err := os.ReadFile("assets/app.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	for _, need := range []string{
		"定制标记表示本分支新增或改动，未标记功能沿用原版",
		"fork-legend",
		"加载全部保留轮次",
		"本机 Agent 文件",
		"后面的轮次",
		"记录与保留",
	} {
		if !strings.Contains(body, need) {
			t.Fatalf("observe page missing %s", need)
		}
	}
	if !strings.Contains(body, `fork-mark","加载全部保留轮次"`) && !strings.Contains(body, `fork-mark","后面的轮次"`) {
		t.Fatal("full-history controls are not marked")
	}
	sess, err := os.ReadFile("../gui/assets/plugins/sessions/client.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(sess)
	if strings.Count(js, "fork-badge") != 1 || !strings.Contains(js, `text fork-mark", t("Show the rest of this conversation")`) {
		t.Fatal("the Sessions sheet should mark only the continued-read control")
	}
	if strings.Contains(js, `fork-mark", t("Show conversation")`) {
		t.Fatal("the original Show conversation control was marked")
	}
}
