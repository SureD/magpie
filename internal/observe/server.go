// Package observe is the web-first session viewer for this fork.
// It reads the gateway's usage ledger and the local conversation store.
// It does not call a model and it does not summarize conversations.
package observe

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed assets/app.html
var assets embed.FS

// Page is the observe UI.
func Page() ([]byte, error) {
	return fs.ReadFile(assets, "assets/app.html")
}

// Server is a started observe page.
type Server struct {
	Addr string
	key  string
	srv  *http.Server
}

// Start listens on addr and serves the page. key is required on every request.
func Start(addr, key string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Addr: ln.Addr().String(), key: key}
	s.srv = &http.Server{Handler: Handler(key), ReadHeaderTimeout: 15 * time.Second}
	go s.srv.Serve(ln)
	return s, nil
}

// Close stops the page.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// Handler is the observe page and its JSON API. Every route requires key.
func Handler(key string) http.Handler {
	mux := http.NewServeMux()
	page, err := Page()
	if err != nil {
		page = []byte("observe page missing")
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		if k := r.URL.Query().Get("k"); k != "" {
			http.SetCookie(w, &http.Cookie{Name: "magpie_observe", Value: k, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	mux.HandleFunc("GET /api/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		writeJSON(w, bootstrap())
	})
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		out, err := listSessions(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/turns", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		out, err := listTurns(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		out, err := search(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/export", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		if err := export(w, r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		writeJSON(w, series(r))
	})
	mux.HandleFunc("POST /api/recording", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		out, code, err := setRecording(r)
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/providers", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		writeJSON(w, listProviders())
	})
	mux.HandleFunc("POST /api/providers", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		out, code, err := addProvider(r)
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		writeJSON(w, out)
	})
	return mux
}

func allow(w http.ResponseWriter, r *http.Request, key string) bool {
	if key == "" || subtle.ConstantTimeCompare([]byte(sentKey(r)), []byte(key)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "需要观测页密钥", http.StatusUnauthorized)
		return false
	}
	return true
}

func sentKey(r *http.Request) string {
	if k := r.URL.Query().Get("k"); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if c, err := r.Cookie("magpie_observe"); err == nil {
		return c.Value
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}
