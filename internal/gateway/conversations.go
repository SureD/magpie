package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/redact"
	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

const conversationBodyMax = 8 << 20

// withGatewaySession tells the client the session its conversation is
// recorded under. Only a session the client named is recorded: magpie never
// makes one up, as text under an id nothing lists can't be read (#1355).
func withGatewaySession(w http.ResponseWriter, r *http.Request) {
	if id := sessionOf(r.Header); id != "" {
		w.Header().Set(SessionHeader, id)
	}
}

func init() {
	WhileServing = append(WhileServing, func(ctx context.Context) {
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			if err := sessions.PruneGatewayConversations(time.Now()); err != nil {
				log.Printf("gateway conversation cleanup failed: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	})
}

type conversationWriter struct {
	*captureResponseWriter
	status int
}

func (w *conversationWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// recordConversation wraps the client-facing path, outside middleware and
// translation, so retries never become duplicate assistant replies.
func recordConversation(w http.ResponseWriter, r *http.Request, proto provider.Protocol, body []byte) (http.ResponseWriter, func()) {
	id := sessionOf(r.Header)
	generation := sessions.GatewayRecordingGeneration()
	if !settings.Shared().GatewayConversations || id == "" {
		return w, func() {}
	}
	at := time.Now()
	// pictures show as [image]: their base64 out first, so a turn of
	// pictures isn't cut past conversationBodyMax (leanImages)
	body = leanImages(body, false)
	input := append([]byte(nil), body[:min(len(body), conversationBodyMax)]...)
	sink := &conversationWriter{captureResponseWriter: &captureResponseWriter{
		ResponseWriter: w, full: &spool{limit: conversationBodyMax, pattern: "magpie-conversation-*"},
	}}
	return sink, func() {
		defer sink.full.discard()
		cut := len(body) > conversationBodyMax || sink.full.cut()
		output, err := sink.full.read()
		if err != nil {
			log.Printf("gateway conversation capture failed: %v", err)
			return
		}
		turn := conversationTurn(proto, input, output)
		turn.ID = sessions.NewTurnID()
		turn.Agent, turn.Session, turn.Time = usage.AgentOf(callerOf(r).agent), id, at
		turn.Project = projectOf(r.Header)
		turn.Normalized = true
		sessions.AddOmit(&turn, sessions.OmitNormalized)
		turn.Status = sink.status
		if turn.Status == 0 {
			turn.Status = http.StatusOK
		}
		turn.Cut = turn.Cut || cut
		if len(body) > conversationBodyMax {
			sessions.AddOmit(&turn, sessions.OmitInputCap)
		}
		if turn.Cut {
			sessions.AddOmit(&turn, sessions.OmitTruncated)
		}
		if partsHaveImage(turn) {
			sessions.AddOmit(&turn, sessions.OmitImages)
		}
		if r.Context().Err() != nil {
			turn.Incomplete = true
			turn.Cancelled = true
			sessions.AddOmit(&turn, sessions.OmitCancelled)
		} else if turn.Cut && !json.Valid(bytes.TrimSpace(output)) {
			turn.Incomplete = true
			sessions.AddOmit(&turn, sessions.OmitIncomplete)
		}
		if box, _ := r.Context().Value(recordedUsageKey{}).(*recordedUsage); box != nil {
			box.mu.Lock()
			if box.has {
				applyRecordedUsage(&turn, box.rec)
			}
			box.mu.Unlock()
		}
		if err := sessions.SaveGatewayTurnGeneration(turn, generation); err != nil {
			log.Printf("gateway conversation save failed: %v", err)
		}
	}
}

// recordedUsage is the ledger row for the request being recorded. It is
// filled by appendUsage and read when the response has finished. Secrets
// and the client address are not copied onto the turn.
type recordedUsage struct {
	mu  sync.Mutex
	has bool
	rec usage.Record
}

type recordedUsageKey struct{}

func noteRecordedUsage(r *http.Request, rec usage.Record) {
	box, _ := r.Context().Value(recordedUsageKey{}).(*recordedUsage)
	if box == nil || rec.IsProbe() {
		return
	}
	box.mu.Lock()
	defer box.mu.Unlock()
	if box.has && rec.HandedOn && !box.rec.HandedOn {
		return
	}
	rec.ClientIP = ""
	rec.ProviderKeyID = ""
	rec.ProviderKeyName = ""
	rec.CallerKeyID = ""
	rec.CallerKeyName = ""
	rec.CallerIssuerID = ""
	rec.Error = ""
	box.rec = rec
	box.has = true
}

func projectOf(h http.Header) string {
	v := strings.TrimSpace(h.Get(ProjectHeader))
	if v == "" {
		return ""
	}
	v = strings.Join(strings.Fields(redact.Scrub(v)), " ")
	if utf8.RuneCountInString(v) > 200 {
		v = string([]rune(v)[:200])
	}
	return v
}

func partsHaveImage(turn sessions.GatewayTurn) bool {
	for _, p := range append(turn.Input, turn.Output...) {
		if p.Kind == "image" || strings.Contains(p.Text, "[image]") {
			return true
		}
	}
	return false
}

func applyRecordedUsage(turn *sessions.GatewayTurn, rec usage.Record) {
	if rec.RequestID != "" {
		turn.RequestID = rec.RequestID
	}
	if rec.RouteID != 0 {
		turn.RouteID = rec.RouteID
	}
	if rec.Status != 0 {
		turn.Status = rec.Status
	}
	if rec.Stop != "" {
		turn.Stop = rec.Stop
	}
	if rec.Model != "" {
		turn.Model = rec.Model
	}
	if rec.Canceled() {
		turn.Cancelled = true
		turn.Incomplete = true
		sessions.AddOmit(turn, sessions.OmitCancelled)
	}
	u := &sessions.TurnUsage{
		Input: rec.Input, Output: rec.Output,
		CacheRead: rec.CacheRead, CacheWrite: rec.CacheWrite, CacheWrite1h: rec.CacheWrite1h,
		CacheUnknown: rec.CacheUnknown, Reasoning: rec.Reasoning,
		Millis: rec.Millis, TTFT: rec.TTFT,
		Provider: rec.Provider, Model: rec.Model,
	}
	if cost, ok := KnownCost(rec); ok {
		u.Cost, u.CostKnown = cost, true
	}
	if n, src, ok := KnownContext(rec.Provider, rec.Model); ok {
		u.Context, u.ContextKnown, u.ContextSource = n, true, src
	}
	turn.Usage = u
}

// KnownCost is the call's price when the client reported one or the
// serving provider's catalog has one. It is not a guess from another vendor.
func KnownCost(rec usage.Record) (float64, bool) {
	if rec.OwnCost != nil {
		return *rec.OwnCost, true
	}
	if rec.Provider == "" {
		return 0, false
	}
	if p, ok := catalog.PricedBy([]string{rec.Provider}, rec.Model); ok {
		return rec.CostAt(p), true
	}
	return 0, false
}

// KnownContext is the context window the user set on the provider, or the
// catalog's published window. source is "provider" or "catalog".
func KnownContext(providerID, model string) (int, string, bool) {
	for _, p := range provider.All() {
		if p.ID != providerID {
			continue
		}
		if n := p.ContextOf(model); n > 0 {
			return n, "provider", true
		}
	}
	if n := catalog.ContextOf(model); n > 0 {
		return n, "catalog", true
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		if n := catalog.ContextOf(model[i+1:]); n > 0 {
			return n, "catalog", true
		}
	}
	return 0, "", false
}

func conversationParts(role string, parts []Part) []sessions.Part {
	var out []sessions.Part
	for _, p := range parts {
		q := sessions.Part{Role: role, Text: p.Text, Kind: string(p.Kind), Name: p.Name}
		switch p.Kind {
		case ToolCall:
			q.Kind, q.Text = "tool_use", string(redact.ScrubJSON(p.Args))
		case ToolResult:
			q.Role = "tool"
			if len(p.Images) > 0 {
				q.Text += "\n[image]"
			}
		case Image:
			q.Text = "[image]"
		case File:
			q.Kind, q.Text = "context", attachmentText(p)
		case Search:
			q.Kind, q.Name = "tool_use", "web_search"
			for _, h := range p.Hits {
				q.Text += "\n" + h.Title + " " + h.URL
			}
		}
		if q.Text != "" {
			out = append(out, q)
		}
	}
	return out
}

func conversationTurn(proto provider.Protocol, input, output []byte) sessions.GatewayTurn {
	t := sessions.GatewayTurn{Input: []sessions.Part{}, Output: []sessions.Part{}}
	input, output = redact.ScrubJSON(input), redact.ScrubJSON(output)
	q, err := parse(proto, input)
	if err != nil {
		t.Cut = true
	} else {
		t.Model = q.Model
		if q.System != "" {
			t.Input = append(t.Input, sessions.Part{Role: "system", Kind: "context", Name: "System prompt", Text: q.System})
		}
		// Keep tool definitions as context, including native tools the IR may
		// not translate; never render embedded images or execute their content.
		var raw map[string]json.RawMessage
		if json.Unmarshal(input, &raw) == nil && len(raw["tools"]) > 0 {
			t.Input = append(t.Input, sessions.Part{Role: "system", Kind: "context", Name: "Tools", Text: string(raw["tools"])})
		}
		for _, m := range q.Messages {
			t.Input = append(t.Input, conversationParts(m.Role, m.Parts)...)
		}
	}
	var cut bool
	t.Output, cut = conversationOutput(proto, output)
	t.Cut = t.Cut || cut
	return t
}

// conversationOutput reads a reply, a whole JSON body or a stream, into
// parts; cut when it couldn't all be read or ended before it was whole.
func conversationOutput(proto provider.Protocol, output []byte) ([]sessions.Part, bool) {
	t := struct {
		Output []sessions.Part
		Cut    bool
	}{Output: []sessions.Part{}}
	var err error
	trim := bytes.TrimSpace(output)
	if json.Valid(trim) {
		t.Output, err = conversationJSON(proto, trim)
		t.Cut = t.Cut || err != nil
	} else if bodyHas(trim, "data:") {
		var c collector
		var completedItems []json.RawMessage
		decode := decoder(proto)
		ended := false
		err = readSSE(bytes.NewReader(output), func(_ string, data string) error {
			if data == "[DONE]" {
				ended = true
				return nil
			}
			if !json.Valid([]byte(data)) {
				t.Cut = true
				return nil
			}
			if proto == provider.Responses {
				var e struct {
					Type     string          `json:"type"`
					Item     json.RawMessage `json:"item"`
					Response struct {
						Output []json.RawMessage `json:"output"`
					} `json:"response"`
				}
				_ = json.Unmarshal([]byte(data), &e)
				if e.Type == "response.output_item.done" {
					completedItems = append(completedItems, e.Item)
				}
				if e.Type == "response.completed" && len(e.Response.Output) > 0 {
					completedItems = e.Response.Output
				}
			}
			return decode(data, func(e Event) {
				if e.Kind == KStop {
					ended = true
				}
				c.add(e)
			})
		})
		t.Output = conversationParts("assistant", c.finish().Parts)
		if proto == provider.Responses && ended && len(completedItems) > 0 {
			b, _ := json.Marshal(map[string]any{"output": completedItems})
			if parts, e := conversationJSON(proto, b); e == nil {
				t.Output = parts
			} else {
				t.Cut = true
			}
		}
		t.Cut = t.Cut || err != nil || !ended
		if c.err != "" {
			t.Output = append(t.Output, sessions.Part{Role: "assistant", Kind: "context", Text: c.err})
		}
	} else if len(trim) > 0 {
		t.Cut = true
	}
	return t.Output, t.Cut
}

// Reuse the request readers for whole replies by placing the actual output in
// their message/input shape. The wire content, not a generated summary, is read.
func conversationJSON(proto provider.Protocol, body []byte) ([]sessions.Part, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if e := raw["error"]; len(e) > 0 && string(e) != "null" {
		return []sessions.Part{{Role: "assistant", Kind: "context", Text: string(e)}}, nil
	}
	var request []byte
	switch proto {
	case provider.Chat:
		var choices []struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(raw["choices"], &choices); err != nil {
			return nil, err
		}
		var messages []json.RawMessage
		for _, c := range choices {
			messages = append(messages, c.Message)
		}
		request, _ = json.Marshal(map[string]any{"messages": messages})
	case provider.Responses:
		request, _ = json.Marshal(map[string]any{"input": raw["output"]})
	case provider.Gemini:
		var c collector
		if err := decoder(proto)(string(body), c.add); err != nil {
			return nil, err
		}
		return conversationParts("assistant", c.finish().Parts), nil
	default:
		request, _ = json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": raw["content"]}}})
	}
	q, err := parse(proto, request)
	if err != nil {
		return nil, err
	}
	var parts []sessions.Part
	for _, m := range q.Messages {
		parts = append(parts, conversationParts("assistant", m.Parts)...)
	}
	if strings.TrimSpace(q.System) != "" {
		parts = append(parts, sessions.Part{Role: "assistant", Kind: "context", Text: q.System})
	}
	return parts, nil
}
