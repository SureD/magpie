package sessions

// Paginated reads of gateway conversations. GatewayTranscript still returns
// the legacy collapsed window (the last 128 exchanges). This file is the
// observe page: one stored turn is one model request, newest page first,
// with a cursor instead of a silent cap.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	hashChainMax   = 20000
	turnPageMax    = 100
	searchReadMax  = 8 << 20
	searchHitsMax  = 200
	exportTurnsMax = 2000

	OmitTruncated    = "truncated"
	OmitPrefixReset  = "prefix_reset"
	OmitChainDropped = "chain_dropped"
	OmitInputCap     = "input_cap"
	OmitImages       = "images_placeholder"
	OmitIncomplete   = "incomplete"
	OmitCancelled    = "cancelled"
	OmitNormalized   = "normalized"
)

// TurnUsage is the usage snapshot taken when the turn was recorded.
// CostKnown and ContextKnown are false when the figure was not reported
// or not in the catalog; a zero then is not a measurement.
type TurnUsage struct {
	Input         int     `json:"in"`
	Output        int     `json:"out"`
	CacheRead     int     `json:"cache_read"`
	CacheWrite    int     `json:"cache_write"`
	CacheWrite1h  int     `json:"cache_write_1h,omitempty"`
	CacheUnknown  bool    `json:"cache_unknown,omitempty"`
	Reasoning     int     `json:"reasoning,omitempty"`
	Millis        int64   `json:"ms,omitempty"`
	TTFT          int64   `json:"ttft_ms,omitempty"`
	Cost          float64 `json:"cost,omitempty"`
	CostKnown     bool    `json:"cost_known"`
	Context       int     `json:"context,omitempty"`
	ContextKnown  bool    `json:"context_known"`
	ContextSource string  `json:"context_source,omitempty"`
	Provider      string  `json:"provider,omitempty"`
	Model         string  `json:"model,omitempty"`
}

// GatewaySessionInfo is the small index beside the turn files.
type GatewaySessionInfo struct {
	Agent      string    `json:"agent"`
	Session    string    `json:"session"`
	Project    string    `json:"project,omitempty"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	Turns      int       `json:"turns"`
	Title      string    `json:"title,omitempty"`
	Cut        int       `json:"cut,omitempty"`
	Incomplete int       `json:"incomplete,omitempty"`
	Cancelled  int       `json:"cancelled,omitempty"`
	Failed     int       `json:"failed,omitempty"`
}

type convState struct {
	Hashes []string `json:"hashes"`
}

// NewTurnID is an id for one recorded request. It is not a session id.
func NewTurnID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("t%x-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

// AddOmit records one omission code on a turn, once.
func AddOmit(t *GatewayTurn, code string) { addOmit(t, code) }

func addOmit(t *GatewayTurn, code string) {
	if code == "" {
		return
	}
	for _, c := range t.Omissions {
		if c == code {
			return
		}
	}
	t.Omissions = append(t.Omissions, code)
}

func partHash(p Part) string {
	h := sha256.New()
	io.WriteString(h, p.Role)
	h.Write([]byte{0})
	io.WriteString(h, p.Kind)
	h.Write([]byte{0})
	io.WriteString(h, p.Name)
	h.Write([]byte{0})
	io.WriteString(h, p.Text)
	return hex.EncodeToString(h.Sum(nil))
}

func splitInput(in []Part) (sys, conv []Part) {
	for _, p := range in {
		if p.Role == "system" {
			sys = append(sys, p)
		} else {
			conv = append(conv, p)
		}
	}
	return sys, conv
}

// foldTurn drops a conversational prefix that matches the last recorded
// state, so a client that resends its history does not store it again.
// It does not invent a match. Caller holds gatewayMu.
func foldTurn(turn *GatewayTurn) convState {
	st := readState(turn.Agent, turn.Session)
	sys, conv := splitInput(turn.Input)
	hashes := make([]string, len(conv))
	for i, p := range conv {
		hashes[i] = partHash(p)
	}
	fresh := conv
	if len(st.Hashes) > 0 && len(hashes) >= len(st.Hashes) && equalHashes(hashes[:len(st.Hashes)], st.Hashes) {
		fresh = conv[len(st.Hashes):]
		if len(st.Hashes) > 0 {
			turn.Delta = true
			turn.Replayed = len(st.Hashes)
		}
	} else if len(st.Hashes) > 0 {
		addOmit(turn, OmitPrefixReset)
	}
	turn.Input = append(sys, fresh...)
	var next []string
	next = append(next, hashes...)
	for _, p := range turn.Output {
		if p.Text == "" && p.Name == "" {
			continue
		}
		next = append(next, partHash(p))
	}
	if len(next) > hashChainMax {
		addOmit(turn, OmitChainDropped)
		return convState{}
	}
	return convState{Hashes: next}
}

func equalHashes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func statePath(agent, id string) string {
	return filepath.Join(gatewayDir(), "state", gatewayIdentity(agent, id)+".json")
}

func indexPath(agent, id string) string {
	return filepath.Join(gatewayDir(), "index", gatewayIdentity(agent, id)+".json")
}

func readState(agent, id string) convState {
	b, err := os.ReadFile(statePath(agent, id))
	if err != nil {
		return convState{}
	}
	var st convState
	if json.Unmarshal(b, &st) != nil {
		return convState{}
	}
	return st
}

// commitTurnSide writes the fold state and the session index. A failure
// here does not remove the turn that was just appended. Caller holds gatewayMu.
func commitTurnSide(turn GatewayTurn, st convState) error {
	if err := writeJSON(statePath(turn.Agent, turn.Session), st); err != nil {
		return err
	}
	return writeIndex(turn)
}

func writeIndex(turn GatewayTurn) error {
	path := indexPath(turn.Agent, turn.Session)
	info := GatewaySessionInfo{Agent: turn.Agent, Session: turn.Session, Project: turn.Project, First: turn.Time, Last: turn.Time}
	if b, err := os.ReadFile(path); err == nil {
		var cur GatewaySessionInfo
		if json.Unmarshal(b, &cur) == nil && cur.Session == turn.Session {
			info = cur
		}
	}
	if info.First.IsZero() || turn.Time.Before(info.First) {
		info.First = turn.Time
	}
	if turn.Time.After(info.Last) {
		info.Last = turn.Time
	}
	info.Turns++
	info.Agent, info.Session = turn.Agent, turn.Session
	if turn.Project != "" {
		info.Project = turn.Project
	}
	if info.Title == "" {
		info.Title = titleOf(turn)
	}
	if turn.Cut {
		info.Cut++
	}
	if turn.Incomplete {
		info.Incomplete++
	}
	if turn.Cancelled {
		info.Cancelled++
	}
	if turn.Status >= 400 {
		info.Failed++
	}
	return writeJSON(path, info)
}

func titleOf(turn GatewayTurn) string {
	for _, p := range turn.Input {
		if p.Role != "user" || p.Kind == "context" || strings.TrimSpace(p.Text) == "" {
			continue
		}
		s := strings.Join(strings.Fields(p.Text), " ")
		if utf8.RuneCountInString(s) > 80 {
			s = string([]rune(s)[:80]) + "…"
		}
		return s
	}
	return ""
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// BackfillGatewayIndex creates index files for turns recorded before the
// index existed. It does not rewrite an index that is already there, and
// it does not delete turn files.
func BackfillGatewayIndex() (int, error) {
	root := gatewayDir()
	days, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	found := map[string]*GatewaySessionInfo{}
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		if _, err := time.Parse(time.DateOnly, day.Name()); err != nil {
			continue
		}
		ids, err := os.ReadDir(filepath.Join(root, day.Name()))
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			if !id.IsDir() {
				continue
			}
			dir := filepath.Join(root, day.Name(), id.Name())
			entries, err := os.ReadDir(dir)
			if err != nil {
				return 0, err
			}
			for _, e := range entries {
				if !e.Type().IsRegular() {
					continue
				}
				path := filepath.Join(dir, e.Name())
				if e.Name() == "turns.jsonl" {
					if err := foldFileInto(found, path); err != nil {
						return 0, err
					}
				} else if strings.HasSuffix(e.Name(), ".json") {
					if err := foldJSONInto(found, path); err != nil {
						return 0, err
					}
				}
			}
		}
	}
	n := 0
	gatewayMu.Lock()
	defer gatewayMu.Unlock()
	for _, info := range found {
		path := indexPath(info.Agent, info.Session)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return n, err
		}
		if err := writeJSON(path, info); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func foldFileInto(found map[string]*GatewaySessionInfo, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	end, err := gatewayCompleteSize(f, info.Size())
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(io.LimitReader(f, end))
	sc.Buffer(make([]byte, 64<<10), gatewayTurnMax+1)
	for sc.Scan() {
		var turn GatewayTurn
		if json.Unmarshal(sc.Bytes(), &turn) != nil {
			return errors.New("unreadable gateway conversation")
		}
		mergeInfo(found, turn)
	}
	return sc.Err()
}

func foldJSONInto(found map[string]*GatewaySessionInfo, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var turn GatewayTurn
	if json.Unmarshal(b, &turn) != nil || turn.Session == "" {
		return nil
	}
	mergeInfo(found, turn)
	return nil
}

func mergeInfo(found map[string]*GatewaySessionInfo, turn GatewayTurn) {
	if turn.Agent == "" || turn.Session == "" {
		return
	}
	key := turn.Agent + "\x00" + turn.Session
	info := found[key]
	if info == nil {
		info = &GatewaySessionInfo{Agent: turn.Agent, Session: turn.Session, First: turn.Time, Last: turn.Time, Title: titleOf(turn)}
		found[key] = info
	}
	info.Turns++
	if turn.Project != "" {
		info.Project = turn.Project
	}
	if info.First.IsZero() || (!turn.Time.IsZero() && turn.Time.Before(info.First)) {
		info.First = turn.Time
	}
	if turn.Time.After(info.Last) {
		info.Last = turn.Time
	}
	if info.Title == "" {
		info.Title = titleOf(turn)
	}
	if turn.Cut {
		info.Cut++
	}
	if turn.Incomplete {
		info.Incomplete++
	}
	if turn.Cancelled {
		info.Cancelled++
	}
	if turn.Status >= 400 {
		info.Failed++
	}
}

// SessionListQuery filters the index. Limit defaults to 40 and is at most 100.
type SessionListQuery struct {
	Agent, Project, Query string
	From, To              time.Time
	Limit                 int
	Cursor                string
}

// SessionList is one page of recorded sessions, newest activity first.
type SessionList struct {
	Sessions []GatewaySessionInfo `json:"sessions"`
	HasMore  bool                 `json:"has_more"`
	Next     string               `json:"next,omitempty"`
}

// ListGatewaySessions reads the index. It backfills from turn files first.
func ListGatewaySessions(q SessionListQuery) (SessionList, error) {
	if _, err := BackfillGatewayIndex(); err != nil {
		return SessionList{}, err
	}
	dir := filepath.Join(gatewayDir(), "index")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return SessionList{Sessions: []GatewaySessionInfo{}}, nil
	}
	if err != nil {
		return SessionList{}, err
	}
	var all []GatewaySessionInfo
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return SessionList{}, err
		}
		var info GatewaySessionInfo
		if json.Unmarshal(b, &info) != nil || info.Session == "" {
			continue
		}
		if q.Agent != "" && info.Agent != q.Agent {
			continue
		}
		if q.Project != "" && info.Project != q.Project {
			continue
		}
		if !q.From.IsZero() && info.Last.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && info.First.After(q.To) {
			continue
		}
		if q.Query != "" && !matchesQuery(info, q.Query) {
			continue
		}
		all = append(all, info)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Last.Equal(all[j].Last) {
			return all[i].Last.After(all[j].Last)
		}
		return all[i].Agent+"/"+all[i].Session < all[j].Agent+"/"+all[j].Session
	})
	limit := q.Limit
	if limit <= 0 {
		limit = 40
	}
	if limit > turnPageMax {
		limit = turnPageMax
	}
	start := 0
	if q.Cursor != "" {
		for i, s := range all {
			if sessionCursor(s) == q.Cursor {
				start = i + 1
				break
			}
		}
	}
	out := SessionList{Sessions: []GatewaySessionInfo{}}
	if start >= len(all) {
		return out, nil
	}
	end := start + limit
	if end < len(all) {
		out.HasMore = true
		out.Sessions = all[start:end]
		out.Next = sessionCursor(out.Sessions[len(out.Sessions)-1])
	} else {
		out.Sessions = all[start:]
	}
	return out, nil
}

func matchesQuery(info GatewaySessionInfo, q string) bool {
	q = strings.ToLower(strings.Join(strings.Fields(q), " "))
	blob := strings.ToLower(info.Agent + " " + info.Session + " " + info.Project + " " + info.Title)
	return strings.Contains(blob, q)
}

func sessionCursor(s GatewaySessionInfo) string {
	return s.Last.UTC().Format(time.RFC3339Nano) + "|" + s.Agent + "|" + s.Session
}

// TurnPage is one chronological page of stored turns.
type TurnPage struct {
	Turns               []GatewayTurn `json:"turns"`
	HasMore             bool          `json:"has_more"`
	NextBefore          time.Time     `json:"next_before,omitempty"`
	NextBeforeID        string        `json:"next_before_id,omitempty"`
	RetentionUnreadable bool          `json:"retention_unreadable,omitempty"`
}

// GatewayTurnPage returns up to limit turns older than the cursor, in
// chronological order. limit defaults to 40 and is clamped to 100.
// The cursor is exclusive: the turn named by before+beforeID is not repeated.
func GatewayTurnPage(agent, id string, before time.Time, beforeID string, limit int) (TurnPage, error) {
	out := TurnPage{Turns: []GatewayTurn{}}
	if agent == "" || id == "" {
		return out, errors.New("no such session")
	}
	if limit <= 0 {
		limit = 40
	}
	if limit > turnPageMax {
		limit = turnPageMax
	}
	p, err := retentionPolicy()
	if err != nil {
		p = defaultPolicy()
		out.RetentionUnreadable = true
	}
	days, err := dayNames(p, time.Now())
	if err != nil {
		return out, err
	}
	var newest []GatewayTurn
	for _, day := range days {
		dir := filepath.Join(gatewayDir(), day, gatewayIdentity(agent, id))
		got, more, err := turnsBefore(dir, agent, id, p, before, beforeID, limit-len(newest)+1)
		if err != nil {
			return out, err
		}
		for _, t := range got {
			if len(newest) == limit {
				out.HasMore = true
				break
			}
			newest = append(newest, t)
		}
		if more && len(newest) >= limit {
			out.HasMore = true
		}
		if len(newest) >= limit {
			break
		}
	}
	for i, j := 0, len(newest)-1; i < j; i, j = i+1, j-1 {
		newest[i], newest[j] = newest[j], newest[i]
	}
	out.Turns = newest
	if out.HasMore && len(newest) > 0 {
		oldest := newest[0]
		out.NextBefore = oldest.Time
		out.NextBeforeID = oldest.ID
	}
	return out, nil
}

// ThreadItem is one stored part in time order. Context says the part is
// not a new message in the running transcript: "system" is repeated
// instructions, "prefix_reset" is a request whose history did not match
// what was already saved, kept whole instead of being merged.
type ThreadItem struct {
	Time       time.Time `json:"time"`
	Role       string    `json:"role"`
	Kind       string    `json:"kind,omitempty"`
	Name       string    `json:"name,omitempty"`
	Text       string    `json:"text"`
	Cut        int       `json:"cut,omitempty"`
	Turn       string    `json:"turn,omitempty"`
	Seq        int       `json:"seq,omitempty"`
	Context    string    `json:"context,omitempty"`
	Incomplete bool      `json:"incomplete,omitempty"`
	Cancelled  bool      `json:"cancelled,omitempty"`
	Status     int       `json:"status,omitempty"`
	Delta      bool      `json:"delta,omitempty"`
	Replayed   int       `json:"replayed,omitempty"`
}

// Thread is one page of a retained conversation, oldest turn first.
type Thread struct {
	Items    []ThreadItem `json:"items"`
	HasMore  bool         `json:"has_more"`
	NextFrom int          `json:"next_from,omitempty"`
	Seen     int          `json:"seen"`
}

// GatewayThread reads retained turns from the oldest, starting at turn
// index from, up to limit turns. It does not drop a prefix_reset or a
// repeated message that was stored as its own part. limit defaults to 40
// and is clamped to 200.
func GatewayThread(agent, id string, from, limit int) (Thread, error) {
	out := Thread{Items: []ThreadItem{}}
	if agent == "" || id == "" {
		return out, errors.New("no such session")
	}
	if from < 0 {
		from = 0
	}
	if limit <= 0 {
		limit = 40
	}
	if limit > 200 {
		limit = 200
	}
	idx := 0
	err := eachGatewayTurn(agent, id, func(t GatewayTurn) bool {
		if idx < from {
			idx++
			return true
		}
		if idx-from >= limit {
			out.HasMore = true
			return false
		}
		start := len(out.Items)
		out.Items = append(out.Items, threadItems(t)...)
		for i := start; i < len(out.Items); i++ {
			out.Items[i].Seq = idx
		}
		out.Seen++
		idx++
		return true
	})
	if out.HasMore {
		out.NextFrom = from + out.Seen
	}
	return out, err
}

// GatewayThreadTranscript is the Sessions page's paged read. from is a
// turn index from the oldest retained turn. Unlike GatewayTranscript it
// does not keep only the last 128. Next is the turn index to ask for
// when Cut is set.
func GatewayThreadTranscript(agent, id string, from int) (Transcript, error) {
	th, err := GatewayThread(agent, id, from, 40)
	out := Transcript{Parts: []Part{}, Source: "gateway"}
	if err != nil {
		return out, err
	}
	size := 0
	fit := 0
	last := -1
	for _, it := range th.Items {
		if it.Seq != last {
			if size >= transcriptMax && fit > 0 {
				out.Cut = true
				out.Next = from + fit
				out.Captured = fit
				return out, nil
			}
			fit++
			last = it.Seq
		}
		out.Parts = append(out.Parts, Part{Role: it.Role, Kind: it.Kind, Name: it.Name, Text: it.Text, Cut: it.Cut})
		size += len(it.Text)
	}
	out.Captured = th.Seen
	if th.HasMore {
		out.Cut = true
		out.Next = th.NextFrom
	}
	return out, nil
}

func threadItems(t GatewayTurn) []ThreadItem {
	reset := false
	for _, c := range t.Omissions {
		if c == OmitPrefixReset {
			reset = true
			break
		}
	}
	var out []ThreadItem
	add := func(p Part, context string) {
		out = append(out, ThreadItem{
			Time: t.Time, Role: p.Role, Kind: p.Kind, Name: p.Name, Text: p.Text, Cut: p.Cut,
			Turn: t.ID, Context: context, Incomplete: t.Incomplete, Cancelled: t.Cancelled,
			Status: t.Status, Delta: t.Delta, Replayed: t.Replayed,
		})
	}
	for _, p := range t.Input {
		ctx := ""
		if p.Role == "system" {
			ctx = "system"
		} else if reset {
			ctx = "prefix_reset"
		}
		add(p, ctx)
	}
	for _, p := range t.Output {
		add(p, "")
	}
	return out
}

// eachGatewayTurn visits retained turns oldest first.
func eachGatewayTurn(agent, id string, visit func(GatewayTurn) bool) error {
	p, err := retentionPolicy()
	if err != nil {
		p = defaultPolicy()
	}
	days, err := dayNames(p, time.Now())
	if err != nil {
		return err
	}
	for i := len(days) - 1; i >= 0; i-- {
		got, err := dayTurns(filepath.Join(gatewayDir(), days[i], gatewayIdentity(agent, id)), agent, id, p)
		if err != nil {
			return err
		}
		sort.SliceStable(got, func(a, b int) bool {
			if !got[a].Time.Equal(got[b].Time) {
				return got[a].Time.Before(got[b].Time)
			}
			return got[a].ID < got[b].ID
		})
		for _, t := range got {
			if !visit(t) {
				return nil
			}
		}
	}
	return nil
}

func dayTurns(dir, agent, id string, p policy) ([]GatewayTurn, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []GatewayTurn
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if e.Name() == "turns.jsonl" {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, err
			}
			end, err := gatewayCompleteSize(f, info.Size())
			if err != nil {
				f.Close()
				return nil, err
			}
			scan := bufio.NewScanner(io.LimitReader(f, end))
			scan.Buffer(make([]byte, 64<<10), gatewayTurnMax+1)
			for scan.Scan() {
				var turn GatewayTurn
				if json.Unmarshal(scan.Bytes(), &turn) != nil {
					f.Close()
					return nil, errors.New("unreadable gateway conversation")
				}
				if turn.Agent == agent && turn.Session == id && !p.expiredTime(turn.Time, time.Now()) {
					out = append(out, turn)
				}
			}
			err = scan.Err()
			f.Close()
			if err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var turn GatewayTurn
		if len(b) > gatewayTurnMax || json.Unmarshal(b, &turn) != nil {
			return nil, errors.New("unreadable gateway conversation")
		}
		if turn.Agent == agent && turn.Session == id && !p.expiredTime(turn.Time, time.Now()) {
			out = append(out, turn)
		}
	}
	return out, nil
}

// turnsBefore reads matching turns newest-first, at most need of them.
// more is true when a further match exists.
func turnsBefore(dir, agent, id string, p policy, before time.Time, beforeID string, need int) ([]GatewayTurn, bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var legacy []GatewayTurn
	var jsonl string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if e.Name() == "turns.jsonl" {
			jsonl = path
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, false, err
			}
			var turn GatewayTurn
			if len(b) > gatewayTurnMax || json.Unmarshal(b, &turn) != nil {
				return nil, false, errors.New("unreadable gateway conversation")
			}
			if keepTurn(turn, agent, id, p, before, beforeID) {
				legacy = append(legacy, turn)
			}
		}
	}
	sort.Slice(legacy, func(i, j int) bool { return laterTurn(legacy[i], legacy[j]) })
	var jsonlTurns []GatewayTurn
	moreFile := false
	if jsonl != "" {
		jsonlTurns, moreFile, err = reverseMatching(jsonl, agent, id, p, before, beforeID, need)
		if err != nil {
			return nil, false, err
		}
	}
	merged := mergeNewest(jsonlTurns, legacy, need)
	more := moreFile || len(jsonlTurns)+len(legacy) > len(merged)
	return merged, more, nil
}

func mergeNewest(a, b []GatewayTurn, need int) []GatewayTurn {
	i, j := 0, 0
	var out []GatewayTurn
	for len(out) < need && (i < len(a) || j < len(b)) {
		takeA := j >= len(b) || (i < len(a) && laterTurn(a[i], b[j]))
		if takeA {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return out
}

func laterTurn(a, b GatewayTurn) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	return a.ID > b.ID
}

func keepTurn(turn GatewayTurn, agent, id string, p policy, before time.Time, beforeID string) bool {
	if turn.Agent != agent || turn.Session != id {
		return false
	}
	if p.expiredTime(turn.Time, time.Now()) {
		return false
	}
	if before.IsZero() {
		return true
	}
	if turn.Time.Before(before) {
		return true
	}
	if turn.Time.Equal(before) && beforeID != "" && turn.ID != "" && turn.ID < beforeID {
		return true
	}
	return false
}

func reverseMatching(path, agent, id string, p policy, before time.Time, beforeID string, need int) ([]GatewayTurn, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	end, err := gatewayCompleteSize(f, info.Size())
	if err != nil {
		return nil, false, err
	}
	var got []GatewayTurn
	more := false
	err = scanReverse(f, end, func(line []byte) bool {
		var turn GatewayTurn
		if json.Unmarshal(line, &turn) != nil {
			return true
		}
		if !keepTurn(turn, agent, id, p, before, beforeID) {
			return true
		}
		if len(got) == need {
			more = true
			return false
		}
		got = append(got, turn)
		return true
	})
	return got, more, err
}

func scanReverse(f *os.File, end int64, visit func([]byte) bool) error {
	const chunk = 256 << 10
	pos := end
	var carry []byte
	for pos > 0 {
		n := chunk
		if int64(n) > pos {
			n = int(pos)
		}
		pos -= int64(n)
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return err
		}
		buf = append(buf, carry...)
		parts := bytes.Split(buf, []byte{'\n'})
		if pos > 0 {
			carry = append([]byte(nil), parts[0]...)
			parts = parts[1:]
			if len(carry) > gatewayTurnMax+1 {
				return errors.New("unreadable gateway conversation")
			}
		} else {
			carry = nil
		}
		for i := len(parts) - 1; i >= 0; i-- {
			if len(parts[i]) == 0 {
				continue
			}
			if !visit(parts[i]) {
				return nil
			}
		}
	}
	return nil
}

func dayNames(p policy, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(gatewayDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var days []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		date, err := time.Parse(time.DateOnly, e.Name())
		if err != nil {
			continue
		}
		if p.expiredDay(date, now) {
			continue
		}
		days = append(days, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days, nil
}

// GatewayHit is one search match. The snippet is the stored text, which
// may already be truncated.
type GatewayHit struct {
	Agent   string    `json:"agent"`
	Session string    `json:"session"`
	Project string    `json:"project,omitempty"`
	Time    time.Time `json:"time"`
	TurnID  string    `json:"turn_id,omitempty"`
	Snippet string    `json:"snippet"`
	Role    string    `json:"role,omitempty"`
}

// SearchGatewayTurns scans recorded turns, newest day first, and stops at
// a byte budget, a hit cap or an error. cut means the scan did not finish.
func SearchGatewayTurns(agent, query string) (hits []GatewayHit, cut bool, err error) {
	q := strings.ToLower(strings.Join(strings.Fields(query), " "))
	if utf8.RuneCountInString(q) < 2 {
		return nil, false, nil
	}
	p, perr := retentionPolicy()
	if perr != nil {
		p = defaultPolicy()
	}
	days, err := dayNames(p, time.Now())
	if err != nil {
		return nil, false, err
	}
	read := 0
	for _, day := range days {
		dir := filepath.Join(gatewayDir(), day)
		idents, err := os.ReadDir(dir)
		if err != nil {
			return hits, true, err
		}
		for _, id := range idents {
			if !id.IsDir() {
				continue
			}
			path := filepath.Join(dir, id.Name(), "turns.jsonl")
			f, err := os.Open(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return hits, true, err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return hits, true, err
			}
			end, err := gatewayCompleteSize(f, info.Size())
			if err != nil {
				f.Close()
				return hits, true, err
			}
			left := searchReadMax - read
			if left <= 0 {
				f.Close()
				return hits, true, nil
			}
			sc := bufio.NewScanner(io.LimitReader(f, min(end, int64(left))))
			sc.Buffer(make([]byte, 64<<10), gatewayTurnMax+1)
			fileCut := end > int64(left)
			for sc.Scan() {
				line := sc.Bytes()
				read += len(line) + 1
				var turn GatewayTurn
				if json.Unmarshal(line, &turn) != nil {
					f.Close()
					return hits, true, errors.New("unreadable gateway conversation")
				}
				if agent != "" && turn.Agent != agent {
					continue
				}
				if p.expiredTime(turn.Time, time.Now()) {
					continue
				}
				if h, ok := matchTurn(turn, q); ok {
					hits = append(hits, h)
					if len(hits) >= searchHitsMax {
						f.Close()
						return hits, true, nil
					}
				}
				if read >= searchReadMax {
					f.Close()
					return hits, true, nil
				}
			}
			err = sc.Err()
			f.Close()
			if err != nil {
				return hits, true, err
			}
			if fileCut {
				return hits, true, nil
			}
		}
	}
	return hits, false, nil
}

func matchTurn(turn GatewayTurn, q string) (GatewayHit, bool) {
	parts := append(append([]Part{}, turn.Input...), turn.Output...)
	for _, p := range parts {
		if p.Role == "system" || p.Kind == "context" {
			continue
		}
		low := strings.ToLower(p.Text)
		i := strings.Index(low, q)
		if i < 0 {
			continue
		}
		return GatewayHit{
			Agent: turn.Agent, Session: turn.Session, Project: turn.Project,
			Time: turn.Time, TurnID: turn.ID, Role: p.Role, Snippet: snippet(p.Text, i, len(q)),
		}, true
	}
	return GatewayHit{}, false
}

func snippet(text string, byteAt, qLen int) string {
	runes := []rune(text)
	at := utf8.RuneCountInString(text[:byteAt])
	qRunes := utf8.RuneCountInString(text[byteAt : byteAt+qLen])
	start := max(0, at-36)
	end := min(len(runes), at+qRunes+120)
	s := string(runes[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(runes) {
		s += "…"
	}
	return s
}

// WriteGatewayHistory writes a Markdown export of retained turns, oldest
// first, up to exportTurnsMax. It says when the export stopped early and
// when a turn was normalized or cut. It is not a byte-for-byte archive.
func WriteGatewayHistory(w io.Writer, agent, id string) error {
	fmt.Fprintf(w, "# 会话 %s / %s\n\n", agent, id)
	fmt.Fprintf(w, "由 Magpie 观测版从网关记录导出。这是规范化后的对话，不是原始报文。图片只保留占位；超长文本会被截断并标明；已知的密钥字段会被擦除，任意正文不会自动脱敏。没有会话标识的请求不会出现在这里。\n\n")
	var before time.Time
	var beforeID string
	seen := 0
	// Pages come newest-first within themselves but each page is chronological.
	// Collect cursors by walking from the newest back, then write in order.
	var pages [][]GatewayTurn
	for seen < exportTurnsMax {
		page, err := GatewayTurnPage(agent, id, before, beforeID, turnPageMax)
		if err != nil {
			return err
		}
		if len(page.Turns) == 0 {
			break
		}
		pages = append(pages, page.Turns)
		seen += len(page.Turns)
		if !page.HasMore || seen >= exportTurnsMax {
			if page.HasMore {
				fmt.Fprintf(w, "> 导出在 %d 轮处停下，更早的轮次仍在磁盘上。\n\n", exportTurnsMax)
			}
			break
		}
		before, beforeID = page.NextBefore, page.NextBeforeID
	}
	for i := len(pages) - 1; i >= 0; i-- {
		for _, turn := range pages[i] {
			writeTurnMarkdown(w, turn)
		}
	}
	if seen == 0 {
		fmt.Fprintf(w, "_没有保留中的对话正文。_\n")
	}
	return nil
}

func writeTurnMarkdown(w io.Writer, turn GatewayTurn) {
	fmt.Fprintf(w, "## %s\n\n", turn.Time.Format(time.RFC3339))
	fmt.Fprintf(w, "- 轮次: `%s`\n", turn.ID)
	if turn.RequestID != "" {
		fmt.Fprintf(w, "- 上游请求: `%s`\n", turn.RequestID)
	}
	if turn.Model != "" {
		fmt.Fprintf(w, "- 模型: `%s`\n", turn.Model)
	}
	fmt.Fprintf(w, "- HTTP: %d\n", turn.Status)
	if turn.Incomplete {
		fmt.Fprintf(w, "- 未完成\n")
	}
	if turn.Cancelled {
		fmt.Fprintf(w, "- 已取消\n")
	}
	if turn.Delta {
		fmt.Fprintf(w, "- 未重复保存此前 %d 段已记录内容\n", turn.Replayed)
	}
	if len(turn.Omissions) > 0 {
		fmt.Fprintf(w, "- 省略标记: %s\n", strings.Join(turn.Omissions, ", "))
	}
	if turn.Usage != nil {
		fmt.Fprintf(w, "- 本次输入上下文 token: %d", turn.Usage.Input)
		if turn.Usage.ContextKnown {
			fmt.Fprintf(w, " / 窗口 %d（%s）", turn.Usage.Context, turn.Usage.ContextSource)
		} else {
			fmt.Fprintf(w, " / 窗口未知")
		}
		fmt.Fprintf(w, "；输出 %d；缓存读 %d；缓存写 %d", turn.Usage.Output, turn.Usage.CacheRead, turn.Usage.CacheWrite)
		if turn.Usage.CostKnown {
			fmt.Fprintf(w, "；费用 %.6f", turn.Usage.Cost)
		} else {
			fmt.Fprintf(w, "；费用未知")
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	writeParts(w, turn.Input)
	writeParts(w, turn.Output)
}

func writeParts(w io.Writer, parts []Part) {
	for _, p := range parts {
		label := p.Role
		if p.Kind != "" && p.Kind != "text" {
			label += "/" + p.Kind
		}
		if p.Name != "" {
			label += " " + p.Name
		}
		fmt.Fprintf(w, "### %s\n\n%s\n", label, p.Text)
		if p.Cut > 0 {
			fmt.Fprintf(w, "\n> 此段末尾省略了 %d 个字符。\n", p.Cut)
		}
		fmt.Fprintln(w)
	}
}
