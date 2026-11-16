package sessions

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGatewayTurnPageWalksEveryRetainedTurn(t *testing.T) {
	gatewayHome(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour)
	const n = 150
	for i := range n {
		turn := GatewayTurn{Agent: "pi", Session: "paged", Time: at.Add(time.Duration(i) * time.Millisecond), Input: []Part{{Role: "user", Kind: "text", Text: fmt.Sprintf("u-%d", i)}}, Output: []Part{{Role: "assistant", Kind: "text", Text: fmt.Sprintf("a-%d", i)}}}
		if err := SaveGatewayTurn(turn); err != nil {
			t.Fatal(err)
		}
	}
	var got []GatewayTurn
	var before time.Time
	var beforeID string
	for {
		page, err := GatewayTurnPage("pi", "paged", before, beforeID, 40)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Turns) == 0 {
			break
		}
		got = append(page.Turns, got...)
		if !page.HasMore {
			break
		}
		before, beforeID = page.NextBefore, page.NextBeforeID
	}
	if len(got) != n {
		t.Fatalf("pages returned %d turns, want %d", len(got), n)
	}
	for i, turn := range got {
		want := fmt.Sprintf("a-%d", i)
		if turn.Output[0].Text != want {
			t.Fatalf("turn %d text %q", i, turn.Output[0].Text)
		}
	}
}

func TestGatewayRetentionKeepsOldTurnsWhenAsked(t *testing.T) {
	gatewayHome(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	turn := GatewayTurn{Agent: "pi", Session: "old", Time: old, Output: []Part{{Role: "assistant", Kind: "text", Text: "kept"}}}
	if err := SaveGatewayTurn(turn); err != nil {
		t.Fatal(err)
	}
	if page, _ := GatewayTurnPage("pi", "old", time.Time{}, "", 10); len(page.Turns) != 0 {
		t.Fatal("default retention stored a 30-day-old turn")
	}
	if err := SetGatewayRetentionForce(0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := SaveGatewayTurn(turn); err != nil {
		t.Fatal(err)
	}
	page, err := GatewayTurnPage("pi", "old", time.Time{}, "", 10)
	if err != nil || len(page.Turns) != 1 || page.Turns[0].Output[0].Text != "kept" {
		t.Fatalf("long retention dropped the turn: %+v %v", page, err)
	}
	if err := PruneGatewayConversations(time.Now()); err != nil {
		t.Fatal(err)
	}
	page, err = GatewayTurnPage("pi", "old", time.Time{}, "", 10)
	if err != nil || len(page.Turns) != 1 {
		t.Fatal("prune deleted a turn under an explicit no-expiry policy")
	}
	if err := SetGatewayRetention(1, 0, 0); err == nil || !ShorterRetention(err) {
		t.Fatal("tightening retention without confirmation was accepted")
	}
	if err := SetGatewayRetentionForce(1, 256, 256); err != nil {
		t.Fatal(err)
	}
	if err := PruneGatewayConversations(time.Now()); err != nil {
		t.Fatal(err)
	}
	page, _ = GatewayTurnPage("pi", "old", time.Time{}, "", 10)
	if len(page.Turns) != 0 {
		t.Fatal("a confirmed one-day policy left a 30-day-old turn readable")
	}
}

func TestGatewayDeltaDoesNotStoreTheReplay(t *testing.T) {
	gatewayHome(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	first := GatewayTurn{Agent: "app", Session: "s", Time: at, Input: []Part{{Role: "user", Kind: "text", Text: "hello"}}, Output: []Part{{Role: "assistant", Kind: "text", Text: "hi"}}}
	if err := SaveGatewayTurn(first); err != nil {
		t.Fatal(err)
	}
	second := GatewayTurn{Agent: "app", Session: "s", Time: at.Add(time.Second), Input: []Part{
		{Role: "user", Kind: "text", Text: "hello"},
		{Role: "assistant", Kind: "text", Text: "hi"},
		{Role: "user", Kind: "text", Text: "next"},
	}, Output: []Part{{Role: "assistant", Kind: "text", Text: "there"}}}
	if err := SaveGatewayTurn(second); err != nil {
		t.Fatal(err)
	}
	page, err := GatewayTurnPage("app", "s", time.Time{}, "", 10)
	if err != nil || len(page.Turns) != 2 {
		t.Fatalf("%+v %v", page, err)
	}
	if !page.Turns[1].Delta || page.Turns[1].Replayed == 0 {
		t.Fatalf("replay was stored again: %+v", page.Turns[1])
	}
	if len(page.Turns[1].Input) != 1 || page.Turns[1].Input[0].Text != "next" {
		t.Fatalf("delta input: %+v", page.Turns[1].Input)
	}
	other := GatewayTurn{Agent: "app", Session: "other", Time: at, Input: []Part{{Role: "user", Kind: "text", Text: "hello"}}, Output: []Part{{Role: "assistant", Kind: "text", Text: "no"}}}
	if err := SaveGatewayTurn(other); err != nil {
		t.Fatal(err)
	}
	page, _ = GatewayTurnPage("app", "other", time.Time{}, "", 10)
	if len(page.Turns) != 1 || page.Turns[0].Delta {
		t.Fatalf("a different session was folded into the first: %+v", page.Turns)
	}
}

func TestGatewaySearchAndExportMarkLimits(t *testing.T) {
	gatewayHome(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := SaveGatewayTurn(GatewayTurn{Agent: "app", Session: "s", Time: at, Input: []Part{{Role: "user", Kind: "text", Text: "synthetic-needle in the prompt"}}, Output: []Part{{Role: "assistant", Kind: "text", Text: "ok"}}}); err != nil {
		t.Fatal(err)
	}
	hits, cut, err := SearchGatewayTurns("app", "synthetic-needle")
	if err != nil || cut || len(hits) != 1 || !strings.Contains(hits[0].Snippet, "synthetic-needle") {
		t.Fatalf("%+v cut=%v %v", hits, cut, err)
	}
	var buf bytes.Buffer
	if err := WriteGatewayHistory(&buf, "app", "s"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "不是原始报文") || !strings.Contains(buf.String(), "synthetic-needle") {
		t.Fatalf("export\n%s", buf.String())
	}
	b, err := os.ReadFile(indexPath("app", "s"))
	if err != nil || !bytes.Contains(b, []byte("synthetic-needle")) {
		t.Fatalf("index: %s %v", b, err)
	}
}

func TestUnreadableRetentionDoesNotPrune(t *testing.T) {
	gatewayHome(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	if err := SaveGatewayTurn(GatewayTurn{Agent: "pi", Session: "s", Time: time.Now(), Output: []Part{{Role: "assistant", Kind: "text", Text: "stay"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retentionPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PruneGatewayConversations(time.Now()); err == nil {
		t.Fatal("unreadable retention was treated as a policy and may have deleted")
	}
	page, err := GatewayTurnPage("pi", "s", time.Time{}, "", 5)
	if err != nil || len(page.Turns) != 1 || !page.RetentionUnreadable {
		t.Fatalf("turn should stay readable and be marked unknown policy: %+v %v", page, err)
	}
}
