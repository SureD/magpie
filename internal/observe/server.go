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

//go:embed assets/app.html assets/fork.css
var assets embed.FS

// Page is the observe UI.
func Page() ([]byte, error) {
	return fs.ReadFile(assets, "assets/app.html")
}

// ForkCSS is the shared custom-mark stylesheet, the same file the desktop
// shell serves as /fork.css.
func ForkCSS() ([]byte, error) {
	return fs.ReadFile(assets, "assets/fork.css")
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

// Mount serves the session pages on the desktop app's own server, and on
// magpie web behind that page's key. No second address and no second secret.
func Mount(mux *http.ServeMux) {
	h := Handler("")
	mux.Handle("/api/observe/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/api" + strings.TrimPrefix(r.URL.Path, "/api/observe")
		if r2.URL.Path == "/api" {
			r2.URL.Path = "/api/"
		}
		h.ServeHTTP(w, r2)
	}))
	mux.HandleFunc("GET /observe", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/observe/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /observe/", func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, "")
	})
}

func servePage(w http.ResponseWriter, r *http.Request, key string) {
	if !allow(w, r, key) {
		return
	}
	if k := r.URL.Query().Get("k"); key != "" && k != "" {
		http.SetCookie(w, &http.Cookie{Name: "magpie_observe", Value: k, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	page, err := Page()
	if err != nil {
		http.Error(w, "observe page missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// Handler is the standalone observe page. A non-empty key is required on
// every route. An empty key is only for Mount, inside the app.
func Handler(key string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		servePage(w, r, key)
	})
	mux.HandleFunc("GET /fork.css", func(w http.ResponseWriter, r *http.Request) {
		b, err := ForkCSS()
		if err != nil {
			http.Error(w, "fork.css missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
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
	mux.HandleFunc("GET /api/thread", func(w http.ResponseWriter, r *http.Request) {
		if !allow(w, r, key) {
			return
		}
		out, err := listThread(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
	// An empty key means the desktop app's own window, or magpie web behind
	// that page's key. The standalone `magpie observe` server always sets one.
	if key == "" {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(sentKey(r)), []byte(key)) != 1 {
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
