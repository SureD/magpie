package observe

import (
	"sort"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/usage"
)

type bucket struct {
	Start       time.Time `json:"start"`
	Calls       int       `json:"calls"`
	Failures    int       `json:"failures"`
	Cancelled   int       `json:"cancelled"`
	Input       int       `json:"billed_input"`
	Output      int       `json:"output"`
	CacheRead   int       `json:"cache_read"`
	CacheWrite  int       `json:"cache_write"`
	Cost        float64   `json:"cost,omitempty"`
	CostKnown   int       `json:"cost_known_calls"`
	CostUnknown int       `json:"cost_unknown_calls"`
	LatencyMS   int64     `json:"latency_ms"`
	LatencyN    int       `json:"latency_n"`
}

type callPoint struct {
	Time          time.Time `json:"t"`
	Agent         string    `json:"agent,omitempty"`
	Session       string    `json:"session,omitempty"`
	RequestID     string    `json:"rid,omitempty"`
	Model         string    `json:"model,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	Status        int       `json:"status"`
	Input         int       `json:"input_context"`
	Output        int       `json:"output"`
	CacheRead     int       `json:"cache_read"`
	CacheWrite    int       `json:"cache_write"`
	CacheUnknown  bool      `json:"cache_unknown,omitempty"`
	Millis        int64     `json:"ms,omitempty"`
	Context       int       `json:"context_window,omitempty"`
	ContextKnown  bool      `json:"context_known"`
	ContextSource string    `json:"context_source,omitempty"`
	Cost          float64   `json:"cost,omitempty"`
	CostKnown     bool      `json:"cost_known"`
	Cancelled     bool      `json:"cancelled,omitempty"`
}

func buildSeries(agent, id, bucketName string) map[string]any {
	if bucketName != "hour" {
		bucketName = "day"
	}
	by := map[string]*bucket{}
	var calls []callPoint
	usage.Visit(time.Time{}, func(rec usage.Record) {
		if rec.IsProbe() || rec.IsReported() || rec.HandedOn {
			return
		}
		if agent != "" && rec.Agent != agent {
			return
		}
		if id != "" && rec.Session != id {
			return
		}
		if id == "" && rec.Session == "" && agent == "" {
			// still count unidentified calls in the global trend, labeled as such
		}
		key := bucketKey(rec.Time, bucketName)
		b := by[key]
		if b == nil {
			b = &bucket{Start: bucketStart(rec.Time, bucketName)}
			by[key] = b
		}
		b.Calls++
		b.Input += rec.Input
		b.Output += rec.Output
		b.CacheRead += rec.CacheRead
		b.CacheWrite += rec.CacheWrite
		if rec.Millis > 0 {
			b.LatencyMS += rec.Millis
			b.LatencyN++
		}
		if rec.Canceled() {
			b.Cancelled++
		} else if rec.Status >= 400 {
			b.Failures++
		}
		if cost, ok := gateway.KnownCost(rec); ok {
			b.Cost += cost
			b.CostKnown++
		} else if rec.Input+rec.Output > 0 {
			b.CostUnknown++
		}
		calls = append(calls, pointOf(rec))
	})
	sort.Slice(calls, func(i, j int) bool { return calls[i].Time.Before(calls[j].Time) })
	if len(calls) > 400 {
		calls = calls[len(calls)-400:]
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buckets := make([]bucket, 0, len(keys))
	for _, k := range keys {
		buckets = append(buckets, *by[k])
	}
	if calls == nil {
		calls = []callPoint{}
	}
	return map[string]any{
		"bucket":  bucketName,
		"buckets": buckets,
		"calls":   calls,
		"note":    "billed_input 是各次请求的计费输入 token 之和，不是上下文长度。calls[].input_context 才是单次请求的输入。context_known 为 false 时窗口未知，图上不会用估计值。费用只在有价格元数据时给出。",
	}
}

func pointOf(rec usage.Record) callPoint {
	p := callPoint{
		Time: rec.Time, Agent: rec.Agent, Session: rec.Session, RequestID: rec.RequestID,
		Model: rec.Model, Provider: rec.Provider, Status: rec.Status,
		Input: rec.Input, Output: rec.Output, CacheRead: rec.CacheRead, CacheWrite: rec.CacheWrite,
		CacheUnknown: rec.CacheUnknown, Millis: rec.Millis, Cancelled: rec.Canceled(),
	}
	if n, src, ok := gateway.KnownContext(rec.Provider, rec.Model); ok {
		p.Context, p.ContextKnown, p.ContextSource = n, true, src
	}
	if cost, ok := gateway.KnownCost(rec); ok {
		p.Cost, p.CostKnown = cost, true
	}
	return p
}

func bucketKey(t time.Time, kind string) string {
	t = t.In(time.Local)
	if kind == "hour" {
		return t.Format("2006-01-02T15")
	}
	return t.Format(time.DateOnly)
}

func bucketStart(t time.Time, kind string) time.Time {
	t = t.In(time.Local)
	if kind == "hour" {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}
