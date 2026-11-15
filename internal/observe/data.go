package observe

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessions"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

const confirmPhrase = "我了解对话正文可能包含源代码与隐私"

func bootstrap() map[string]any {
	return map[string]any{
		"recording":      recordingOn(),
		"retention":      sessions.GatewayRetentionView(),
		"gateway":        gateway.URL(),
		"confirm_phrase": confirmPhrase,
		"limits": []string{
			"对话正文默认不记录。打开记录前需要输入确认语。记录只留在这台电脑。",
			"只有客户端自己带上的会话标识才会归档。没有 X-Magpie-Session（或该客户端自己的会话头）的请求不会被猜成一段对话。",
			"项目名只来自 X-Magpie-Project，不会从目录或提示词推断。",
			"保存的是规范化后的用户、助手和工具内容，不是逐字节原始报文。图片只留 [image] 占位。单段文本最多 64 KiB，单轮最多 1 MiB。",
			"已知名称的密钥字段会被擦除。Authorization、Cookie 和令牌不会写入对话。任意正文里的隐私不会自动擦掉，否则就无法同时承诺内容完整。",
			"客户端重发的已记录历史不会再存一份。对不上的上下文会整段留下，并标成 prefix_reset，不会被拼成一条猜出来的历史。",
			"未配置时沿用原来的 7 天和 256 MiB。天数或容量设为 0 表示该项不自动删除。收紧策略需要再次确认，因为下次清理会删掉超出的内容。",
			"旧的会话页接口仍只展开最近 128 轮。这个观测页按页读取保留期内的每一轮。",
			"趋势里的「累计输入」是各次请求计费输入之和。单次「输入上下文」才是那一次请求带上的提示词 token。窗口未知时不会填一个估计值。",
		},
	}
}

func recordingOn() bool { return settings.Shared().GatewayConversations }

type sessionRow struct {
	Agent      string    `json:"agent"`
	ID         string    `json:"id"`
	Project    string    `json:"project,omitempty"`
	Title      string    `json:"title,omitempty"`
	Start      time.Time `json:"start,omitempty"`
	Last       time.Time `json:"last,omitempty"`
	Turns      int       `json:"turns,omitempty"`
	Text       bool      `json:"text"`
	Input      int       `json:"billed_input"`
	Output     int       `json:"output"`
	CacheRead  int       `json:"cache_read"`
	CacheWrite int       `json:"cache_write"`
	Cost       float64   `json:"cost,omitempty"`
	Unpriced   int       `json:"unpriced,omitempty"`
	UsageKnown bool      `json:"usage_known"`
	Models     []string  `json:"models,omitempty"`
	Incomplete int       `json:"incomplete,omitempty"`
	Failed     int       `json:"failed,omitempty"`
	Cancelled  int       `json:"cancelled,omitempty"`
}

type sessionList struct {
	Sessions []sessionRow `json:"sessions"`
	HasMore  bool         `json:"has_more"`
	Next     string       `json:"next,omitempty"`
	Agents   []string     `json:"agents"`
	Projects []string     `json:"projects"`
}

func listSessions(r *http.Request) (sessionList, error) {
	q := r.URL.Query()
	rows := map[string]*sessionRow{}
	usage.EachGatewaySession(nil, func(s usage.GatewaySession) bool {
		row := &sessionRow{
			Agent: s.Agent, ID: s.ID, Start: s.Start, Last: s.Last,
			Input: s.Input, Output: s.Output, CacheRead: s.CacheRead, CacheWrite: s.CacheWrite,
			Cost: s.Cost, Unpriced: s.Unpriced, UsageKnown: true,
		}
		for _, m := range s.Models {
			row.Models = append(row.Models, m.Model)
		}
		rows[s.Agent+"\x00"+s.ID] = row
		return true
	})
	listed, err := sessions.ListGatewaySessions(sessions.SessionListQuery{Limit: 100})
	if err != nil {
		return sessionList{}, err
	}
	// List is paged; walk cursors so the merge sees every indexed session.
	for {
		for _, info := range listed.Sessions {
			key := info.Agent + "\x00" + info.Session
			row := rows[key]
			if row == nil {
				row = &sessionRow{Agent: info.Agent, ID: info.Session}
				rows[key] = row
			}
			row.Text = true
			row.Project = info.Project
			row.Title = info.Title
			row.Turns = info.Turns
			row.Incomplete = info.Incomplete
			row.Failed = info.Failed
			row.Cancelled = info.Cancelled
			if row.Start.IsZero() || (!info.First.IsZero() && info.First.Before(row.Start)) {
				row.Start = info.First
			}
			if info.Last.After(row.Last) {
				row.Last = info.Last
			}
		}
		if !listed.HasMore {
			break
		}
		listed, err = sessions.ListGatewaySessions(sessions.SessionListQuery{Limit: 100, Cursor: listed.Next})
		if err != nil {
			return sessionList{}, err
		}
	}
	var all []*sessionRow
	agents := map[string]bool{}
	projects := map[string]bool{}
	for _, row := range rows {
		agents[row.Agent] = true
		if row.Project != "" {
			projects[row.Project] = true
		}
		all = append(all, row)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Last.Equal(all[j].Last) {
			return all[i].Last.After(all[j].Last)
		}
		return all[i].Agent+"/"+all[i].ID < all[j].Agent+"/"+all[j].ID
	})
	agent := q.Get("agent")
	project := q.Get("project")
	query := strings.ToLower(strings.TrimSpace(q.Get("q")))
	from := parseTime(q.Get("from"))
	to := parseTime(q.Get("to"))
	var filtered []sessionRow
	for _, row := range all {
		if agent != "" && row.Agent != agent {
			continue
		}
		if project != "" && row.Project != project {
			continue
		}
		if !from.IsZero() && row.Last.Before(from) {
			continue
		}
		if !to.IsZero() && row.Start.After(to) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(row.Agent+" "+row.ID+" "+row.Project+" "+row.Title), query) {
			continue
		}
		filtered = append(filtered, *row)
	}
	if filtered == nil {
		filtered = []sessionRow{}
	}
	limit := atoi(q.Get("limit"), 40)
	if limit > 100 {
		limit = 100
	}
	start := 0
	if c := q.Get("cursor"); c != "" {
		for i, row := range filtered {
			if row.Agent+"|"+row.ID == c {
				start = i + 1
				break
			}
		}
	}
	out := sessionList{Sessions: []sessionRow{}, Agents: keys(agents), Projects: keys(projects)}
	if start >= len(filtered) {
		return out, nil
	}
	end := start + limit
	if end < len(filtered) {
		out.HasMore = true
		out.Sessions = filtered[start:end]
		last := out.Sessions[len(out.Sessions)-1]
		out.Next = last.Agent + "|" + last.ID
	} else {
		out.Sessions = filtered[start:]
	}
	return out, nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func listTurns(r *http.Request) (sessions.TurnPage, error) {
	q := r.URL.Query()
	agent, id := q.Get("agent"), q.Get("id")
	if agent == "" || id == "" {
		return sessions.TurnPage{}, errors.New("agent and id are required")
	}
	before := parseTime(q.Get("before"))
	return sessions.GatewayTurnPage(agent, id, before, q.Get("before_id"), atoi(q.Get("limit"), 40))
}

func search(r *http.Request) (map[string]any, error) {
	hits, cut, err := sessions.SearchGatewayTurns(r.URL.Query().Get("agent"), r.URL.Query().Get("q"))
	if err != nil {
		return nil, err
	}
	if hits == nil {
		hits = []sessions.GatewayHit{}
	}
	return map[string]any{"hits": hits, "cut": cut, "note": "搜索的是已保存的规范化文本，不含系统提示和工具定义。cut 为 true 时扫描因篇幅上限停下，不是没有更多匹配。"}, nil
}

func export(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	agent, id := q.Get("agent"), q.Get("id")
	if agent == "" || id == "" {
		return errors.New("agent and id are required")
	}
	format := q.Get("format")
	if format == "" {
		format = "md"
	}
	switch format {
	case "md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=session.md")
		return sessions.WriteGatewayHistory(w, agent, id)
	case "jsonl":
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=session.jsonl")
		meta, _ := json.Marshal(map[string]any{
			"_observe": "normalized",
			"lossy":    true,
			"notes":    []string{"不是逐字节原文", "图片为占位", "超长文本有 cut", "密钥字段已擦除", "重复历史已折叠"},
		})
		if _, err := w.Write(append(meta, '\n')); err != nil {
			return err
		}
		return writeJSONLPages(w, agent, id)
	default:
		return errors.New("format is md or jsonl")
	}
}

func writeJSONLPages(w http.ResponseWriter, agent, id string) error {
	var pages [][]sessions.GatewayTurn
	var before time.Time
	var beforeID string
	n := 0
	for n < 2000 {
		page, err := sessions.GatewayTurnPage(agent, id, before, beforeID, 100)
		if err != nil {
			return err
		}
		if len(page.Turns) == 0 {
			break
		}
		pages = append(pages, page.Turns)
		n += len(page.Turns)
		if !page.HasMore {
			break
		}
		before, beforeID = page.NextBefore, page.NextBeforeID
	}
	enc := json.NewEncoder(w)
	for i := len(pages) - 1; i >= 0; i-- {
		for _, turn := range pages[i] {
			if err := enc.Encode(turn); err != nil {
				return err
			}
		}
	}
	return nil
}

type recordReq struct {
	On             *bool  `json:"on"`
	Clear          bool   `json:"clear"`
	Confirm        string `json:"confirm"`
	ConfirmShorten bool   `json:"confirm_shorten"`
	Days           *int   `json:"days"`
	StoreMB        *int   `json:"store_mb"`
	SessionMB      *int   `json:"session_mb"`
}

func setRecording(r *http.Request) (any, int, error) {
	var req recordReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, errors.New("请求不是 JSON")
	}
	if req.On != nil && *req.On || req.Clear {
		if req.Confirm != confirmPhrase {
			return nil, http.StatusBadRequest, errors.New("打开记录或清除正文需要确认语：" + confirmPhrase)
		}
	}
	if req.Clear {
		if err := sessions.SetGatewayRecording(false, true); err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}
	if req.On != nil {
		if err := sessions.SetGatewayRecording(*req.On, false); err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}
	if req.Days != nil || req.StoreMB != nil || req.SessionMB != nil {
		cur := sessions.GatewayRetentionView()
		days, store, session := cur.Days, cur.StoreMB, cur.SessionMB
		if cur.Forever {
			days = 0
		}
		if cur.StoreUnlimited {
			store = 0
		}
		if cur.SessionUnlimited {
			session = 0
		}
		if req.Days != nil {
			days = *req.Days
		}
		if req.StoreMB != nil {
			store = *req.StoreMB
		}
		if req.SessionMB != nil {
			session = *req.SessionMB
		}
		var err error
		if req.ConfirmShorten {
			err = sessions.SetGatewayRetentionForce(days, store, session)
		} else {
			err = sessions.SetGatewayRetention(days, store, session)
		}
		if sessions.ShorterRetention(err) {
			return nil, http.StatusConflict, errors.New("新策略会更早删除已保存的对话。确认后请带 confirm_shorten: true 再提交")
		}
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
	}
	return bootstrap(), http.StatusOK, nil
}

func listProviders() map[string]any {
	var out []map[string]any
	for _, p := range provider.All() {
		item := map[string]any{
			"id": p.ID, "name": p.Name, "preset": p.Preset,
			"has_key": strings.TrimSpace(p.Key) != "",
			"models":  p.Models,
			"chat":    p.Chat, "responses": p.Responses, "anthropic": p.Anthropic, "gemini": p.Gemini,
		}
		if n := p.ContextOf("*"); n > 0 {
			item["context"] = n
			item["context_source"] = "provider"
		}
		out = append(out, item)
	}
	if out == nil {
		out = []map[string]any{}
	}
	return map[string]any{"providers": out}
}

type providerIn struct {
	Name     string   `json:"name"`
	Protocol string   `json:"protocol"`
	BaseURL  string   `json:"base_url"`
	Key      string   `json:"key"`
	Models   []string `json:"models"`
	Context  int      `json:"context"`
}

func addProvider(r *http.Request) (any, int, error) {
	var in providerIn
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&in); err != nil {
		return nil, http.StatusBadRequest, errors.New("请求不是 JSON")
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Key = strings.TrimSpace(in.Key)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	if in.Name == "" || in.Key == "" || in.BaseURL == "" {
		return nil, http.StatusBadRequest, errors.New("需要 name、base_url 和 key")
	}
	u, err := url.Parse(in.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, http.StatusBadRequest, errors.New("base_url 必须是 http 或 https 地址")
	}
	id := provider.Slug(in.Name)
	for _, p := range provider.All() {
		if p.ID == id {
			return nil, http.StatusConflict, errors.New("已经有同名 provider：" + id)
		}
	}
	p := provider.Provider{ID: id, Name: in.Name, Key: in.Key, Models: compactModels(in.Models), BaseAPI: in.Protocol}
	switch in.Protocol {
	case "", "chat":
		p.Chat = in.BaseURL
		p.BaseAPI = "chat"
	case "responses":
		p.Responses = in.BaseURL
	case "anthropic":
		p.Anthropic = in.BaseURL
	case "gemini":
		p.Gemini = in.BaseURL
	default:
		return nil, http.StatusBadRequest, errors.New("protocol 是 chat、responses、anthropic 或 gemini")
	}
	if in.Context > 0 {
		p.Contexts = map[string]int{"*": in.Context}
	}
	if err := provider.Save(p); err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"id": id, "saved": true}, http.StatusOK, nil
}

func compactModels(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func atoi(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t
	}
	return time.Time{}
}

// series is the token and context trend for one session, or every session
// when id is empty.
func series(r *http.Request) map[string]any {
	q := r.URL.Query()
	return buildSeries(q.Get("agent"), q.Get("id"), q.Get("bucket"))
}
