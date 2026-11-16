package sessions

import (
	"strings"
	"testing"
	"time"
)

func saveAt(t *testing.T, turn GatewayTurn) {
	t.Helper()
	if err := SaveGatewayTurn(turn); err != nil {
		t.Fatal(err)
	}
}

func TestThreadKeepsLongPartsAndEveryTurn(t *testing.T) {
	gatewayHome(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour)
	big := strings.Repeat("x", 30_000)
	saveAt(t, GatewayTurn{
		Agent: "app", Session: "long", Time: at,
		Output: []Part{
			{Role: "assistant", Kind: "text", Text: big},
			{Role: "assistant", Kind: "text", Text: big},
			{Role: "assistant", Kind: "text", Text: big},
		},
	})
	th, err := GatewayThread("app", "long", 0, 10)
	if err != nil || len(th.Items) != 3 {
		t.Fatalf("three parts should survive a 64KiB total: %+v %v", th, err)
	}
	for _, it := range th.Items {
		if it.Cut != 0 || len(it.Text) < 30_000 {
			t.Fatalf("part was cut: %+v", it)
		}
	}
	huge := strings.Repeat("y", partTextMax+100)
	saveAt(t, GatewayTurn{Agent: "app", Session: "long", Time: at.Add(time.Second), Output: []Part{{Role: "assistant", Kind: "text", Text: huge}}})
	th, err = GatewayThread("app", "long", 0, 10)
	if err != nil || len(th.Items) != 4 || th.Items[3].Cut == 0 || len(th.Items[3].Text) > partTextMax {
		t.Fatalf("over-long part should be marked cut: %+v %v", th.Items, err)
	}

	base := at.Add(-48 * time.Hour)
	for i := range 150 {
		saveAt(t, GatewayTurn{
			Agent: "app", Session: "many", Time: base.Add(time.Duration(i) * time.Minute),
			Input:  []Part{{Role: "user", Kind: "text", Text: "u"}},
			Output: []Part{{Role: "assistant", Kind: "text", Text: "a"}},
		})
	}
	seen := 0
	from := 0
	for {
		page, err := GatewayThread("app", "many", from, 40)
		if err != nil {
			t.Fatal(err)
		}
		seen += page.Seen
		if !page.HasMore {
			break
		}
		if page.NextFrom <= from {
			t.Fatalf("cursor did not move: %d", page.NextFrom)
		}
		from = page.NextFrom
	}
	if seen != 150 {
		t.Fatalf("saw %d of 150 turns", seen)
	}
	legacy, err := GatewayTranscript("app", "many")
	if err != nil || legacy.Captured != gatewayReadMax || !legacy.Cut {
		t.Fatalf("legacy window changed: %+v %v", legacy, err)
	}
	full, err := GatewayThreadTranscript("app", "many", 0)
	if err != nil || !full.Cut || full.Next == 0 || full.Captured >= 150 {
		t.Fatalf("paged transcript should continue: %+v %v", full, err)
	}
	rest, err := GatewayThreadTranscript("app", "many", full.Next)
	if err != nil || rest.Captured == 0 {
		t.Fatalf("next page empty: %+v %v", rest, err)
	}
}

func TestThreadKeepsRepeatsResendAndFork(t *testing.T) {
	gatewayHome(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour)
	user := Part{Role: "user", Kind: "text", Text: "ping"}
	asst := Part{Role: "assistant", Kind: "text", Text: "pong"}
	saveAt(t, GatewayTurn{Agent: "app", Session: "s", Time: at, Input: []Part{user}, Output: []Part{asst}})
	// The client sends the same user text again as a new message, after the reply.
	saveAt(t, GatewayTurn{Agent: "app", Session: "s", Time: at.Add(time.Second), Input: []Part{user, asst, user}, Output: []Part{asst}})
	// A fork: the history the client sends is not the saved prefix.
	saveAt(t, GatewayTurn{Agent: "app", Session: "s", Time: at.Add(2 * time.Second), Input: []Part{{Role: "user", Kind: "text", Text: "other"}}, Output: []Part{{Role: "assistant", Kind: "text", Text: "forked"}}})
	th, err := GatewayThread("app", "s", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	resets := 0
	for _, it := range th.Items {
		if it.Context == "prefix_reset" {
			resets++
		}
		if it.Role == "user" {
			texts = append(texts, it.Text)
		}
	}
	if len(texts) != 3 || texts[0] != "ping" || texts[1] != "ping" || texts[2] != "other" {
		t.Fatalf("user texts: %v", texts)
	}
	if resets == 0 {
		t.Fatal("fork was merged or dropped")
	}
}

func TestThreadToolRoundAndIncomplete(t *testing.T) {
	gatewayHome(t)
	if err := SetGatewayRecording(true, false); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour)
	saveAt(t, GatewayTurn{
		Agent: "app", Session: "tools", Time: at, Status: 200,
		Input:  []Part{{Role: "user", Kind: "text", Text: "run pwd"}},
		Output: []Part{{Role: "assistant", Kind: "tool_use", Name: "bash", Text: `{"command":"pwd"}`}},
	})
	saveAt(t, GatewayTurn{
		Agent: "app", Session: "tools", Time: at.Add(time.Second), Status: 200,
		Input: []Part{
			{Role: "user", Kind: "text", Text: "run pwd"},
			{Role: "assistant", Kind: "tool_use", Name: "bash", Text: `{"command":"pwd"}`},
			{Role: "tool", Kind: "tool_result", Name: "bash", Text: "/tmp"},
		},
		Output:     []Part{{Role: "assistant", Kind: "text", Text: "it is /tmp"}},
		Incomplete: true,
	})
	th, err := GatewayThread("app", "tools", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	incomplete := false
	for _, it := range th.Items {
		kinds = append(kinds, it.Kind+":"+it.Text)
		incomplete = incomplete || it.Incomplete
	}
	want := []string{"text:run pwd", "tool_use:{\"command\":\"pwd\"}", "tool_result:/tmp", "text:it is /tmp"}
	if strings.Join(kinds, "|") != strings.Join(want, "|") || !incomplete {
		t.Fatalf("round: %v incomplete=%v", kinds, incomplete)
	}
}
