package sessions

// Retention of gateway conversations is local to this computer. A missing
// file keeps the historical default (7 days, 256 MiB). Zero means no limit
// of that kind. An unreadable file is not treated as "keep forever": prune
// deletes nothing until it can be read.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

const retentionName = "gateway-retention.json"

// policy is the effective limit. window is ignored when forever is set.
// storeMax and sessionMax of 0 mean that cap is not applied. Per-turn and
// per-part caps in gateway.go still hold.
type policy struct {
	window     time.Duration
	forever    bool
	storeMax   int64
	sessionMax int64
	explicit   bool
}

func defaultPolicy() policy {
	return policy{window: GatewayRetention, storeMax: gatewayStoreMax, sessionMax: gatewaySessionMax}
}

type retentionFile struct {
	Days      *int `json:"days"`
	StoreMB   *int `json:"store_mb"`
	SessionMB *int `json:"session_mb"`
}

func (f retentionFile) policy() policy {
	p := defaultPolicy()
	p.explicit = true
	if f.Days != nil {
		if *f.Days <= 0 {
			p.forever = true
			p.window = 0
		} else {
			p.forever = false
			p.window = time.Duration(*f.Days) * 24 * time.Hour
		}
	}
	if f.StoreMB != nil {
		if *f.StoreMB <= 0 {
			p.storeMax = 0
		} else {
			p.storeMax = int64(*f.StoreMB) << 20
		}
	}
	if f.SessionMB != nil {
		if *f.SessionMB <= 0 {
			p.sessionMax = 0
		} else {
			p.sessionMax = int64(*f.SessionMB) << 20
		}
	}
	return p
}

func retentionPath() string { return filepath.Join(settings.Dir(), retentionName) }

// retentionPolicy reads the local file. A missing file is the default.
// Any other error is returned so a prune does not delete on a guess.
func retentionPolicy() (policy, error) {
	b, err := os.ReadFile(retentionPath())
	if errors.Is(err, os.ErrNotExist) {
		return defaultPolicy(), nil
	}
	if err != nil {
		return policy{}, err
	}
	var f retentionFile
	if json.Unmarshal(b, &f) != nil {
		return policy{}, errors.New("gateway retention file is unreadable")
	}
	return f.policy(), nil
}

func (p policy) expiredDay(date, now time.Time) bool {
	if p.forever {
		return false
	}
	return !date.Add(24 * time.Hour).After(now.Add(-p.window))
}

func (p policy) expiredTime(t, now time.Time) bool {
	if p.forever || t.IsZero() {
		return false
	}
	return t.Before(now.Add(-p.window))
}

func (p policy) stricterThan(old policy) bool {
	if old.forever && !p.forever {
		return true
	}
	if !old.forever && !p.forever && p.window < old.window {
		return true
	}
	if old.storeMax == 0 && p.storeMax > 0 {
		return true
	}
	if old.storeMax > 0 && p.storeMax > 0 && p.storeMax < old.storeMax {
		return true
	}
	if old.sessionMax == 0 && p.sessionMax > 0 {
		return true
	}
	if old.sessionMax > 0 && p.sessionMax > 0 && p.sessionMax < old.sessionMax {
		return true
	}
	return false
}

// RetentionView is the policy the observe page shows.
type RetentionView struct {
	Days             int    `json:"days"`
	Forever          bool   `json:"forever"`
	StoreMB          int    `json:"store_mb"`
	StoreUnlimited   bool   `json:"store_unlimited"`
	SessionMB        int    `json:"session_mb"`
	SessionUnlimited bool   `json:"session_unlimited"`
	Explicit         bool   `json:"explicit"`
	Unreadable       bool   `json:"unreadable,omitempty"`
	Note             string `json:"note,omitempty"`
}

func viewOf(p policy, unreadable bool) RetentionView {
	v := RetentionView{
		Forever: p.forever, Explicit: p.explicit, Unreadable: unreadable,
		StoreUnlimited: p.storeMax == 0, SessionUnlimited: p.sessionMax == 0,
	}
	if p.forever {
		v.Days = 0
	} else if p.window > 0 {
		v.Days = int(p.window / (24 * time.Hour))
	}
	if p.storeMax > 0 {
		v.StoreMB = int(p.storeMax >> 20)
	}
	if p.sessionMax > 0 {
		v.SessionMB = int(p.sessionMax >> 20)
	}
	if unreadable {
		v.Note = "保留策略文件无法读取。为避免误删，清理已暂停；读取仍按默认的 7 天窗口，磁盘上的更早内容还在。"
		v.Explicit = false
	} else if !p.explicit {
		v.Note = "尚未单独配置，沿用默认：保留 7 天，总量与单日会话文件各 256 MiB。设为 0 表示该项不按时间或容量自动删除。"
	}
	return v
}

// GatewayRetentionView reports the effective policy.
func GatewayRetentionView() RetentionView {
	p, err := retentionPolicy()
	if err != nil {
		return viewOf(defaultPolicy(), true)
	}
	return viewOf(p, false)
}

// SetGatewayRetention writes this computer's policy. It does not delete
// conversations. days, storeMB and sessionMB of 0 mean no limit of that kind.
func SetGatewayRetention(days, storeMB, sessionMB int) error {
	if days < 0 || storeMB < 0 || sessionMB < 0 {
		return errors.New("retention values must be zero or positive")
	}
	if days > 36500 || storeMB > 1<<20 || sessionMB > 1<<20 {
		return errors.New("retention value is unreasonably large")
	}
	cur, err := retentionPolicy()
	if err != nil {
		return fmt.Errorf("read retention before changing it: %w", err)
	}
	next := retentionFile{Days: &days, StoreMB: &storeMB, SessionMB: &sessionMB}.policy()
	if next.stricterThan(cur) {
		return errShorterRetention
	}
	return writeRetention(days, storeMB, sessionMB)
}

// errShorterRetention is returned when a new policy can delete sooner.
var errShorterRetention = errors.New("shorter retention")

// ShorterRetention reports whether err is a refusal to tighten retention
// without an explicit confirmation.
func ShorterRetention(err error) bool { return errors.Is(err, errShorterRetention) }

// SetGatewayRetentionForce writes the policy even when it can delete sooner.
func SetGatewayRetentionForce(days, storeMB, sessionMB int) error {
	if days < 0 || storeMB < 0 || sessionMB < 0 {
		return errors.New("retention values must be zero or positive")
	}
	if days > 36500 || storeMB > 1<<20 || sessionMB > 1<<20 {
		return errors.New("retention value is unreasonably large")
	}
	return writeRetention(days, storeMB, sessionMB)
}

func writeRetention(days, storeMB, sessionMB int) error {
	if err := os.MkdirAll(settings.Dir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(retentionFile{Days: &days, StoreMB: &storeMB, SessionMB: &sessionMB}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(retentionPath(), append(b, '\n'), 0o600)
}

func activePolicy() policy {
	p, err := retentionPolicy()
	if err != nil {
		return defaultPolicy()
	}
	return p
}
