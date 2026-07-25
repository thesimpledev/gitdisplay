// Package server serves the rendered cards.
//
// Every response is read straight out of memory. Nothing on the request path
// talks to GitHub, touches the disk, or takes a write lock, so a request costs
// a read lock and a copy.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/thesimpledev/gitdisplay/internal/cache"
	"github.com/thesimpledev/gitdisplay/internal/config"
)

// Card names, also used as the on-disk filenames.
const (
	CardLangs  = "top-langs"
	CardStreak = "streak"
)

// Server wires the store to the HTTP routes.
type Server struct {
	store *cache.Store
	cfg   config.Config
	// startedAt is reported by /healthz.
	startedAt time.Time
	// version is reported by /healthz.
	version string
}

// New builds the server.
func New(store *cache.Store, cfg config.Config, version string) *Server {
	return &Server{store: store, cfg: cfg, startedAt: time.Now().UTC(), version: version}
}

// HSTS is only sent over TLS. Sending it on a plain-HTTP deployment would
// pin browsers to a scheme this service is not serving.
func (s *Server) hsts() string {
	if !s.cfg.Server.TLSEnabled() {
		return ""
	}
	return "max-age=31536000; includeSubDomains"
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	if s.cfg.Languages.Enabled {
		mux.HandleFunc("GET /top-langs.svg", s.card(CardLangs, s.cfg.Server.LangsMaxAge.D()))
	}
	if s.cfg.Streak.Enabled {
		mux.HandleFunc("GET /streak.svg", s.card(CardStreak, s.cfg.Server.StreakMaxAge.D()))
	}
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /{$}", s.index)

	return securityHeaders(mux, s.hsts())
}

func (s *Server) card(name string, maxAge time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := s.store.Snapshot(name)
		if snap == nil {
			// Nothing has ever rendered. Say so plainly rather than
			// returning a broken image; a README shows the alt text.
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "card is not ready yet", http.StatusServiceUnavailable)
			return
		}

		body := snap.SVG
		etag := snap.ETag
		if acceptsGzip(r) && len(snap.Gzip) > 0 {
			body = snap.Gzip
			etag = gzipETag(snap.ETag)
			w.Header().Set("Content-Encoding", "gzip")
		}

		h := w.Header()
		h.Set("Content-Type", "image/svg+xml; charset=utf-8")
		h.Set("Vary", "Accept-Encoding")
		h.Set("ETag", etag)
		h.Set("Last-Modified", snap.RenderedAt.UTC().Format(http.TimeFormat))
		// See doc/CACHING.md. GitHub proxies README images through camo,
		// which keeps its own copy on top of this. A short max-age plus a
		// cheap revalidation is what actually gets a new card through;
		// immutable or long max-age values do not.
		h.Set("Cache-Control", fmt.Sprintf(
			"public, max-age=%d, s-maxage=%d, must-revalidate", int(maxAge.Seconds()), int(maxAge.Seconds())))

		if matchesETag(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		h.Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}
}

type health struct {
	Status    string                  `json:"status"`
	Version   string                  `json:"version"`
	StartedAt time.Time               `json:"started_at"`
	Now       time.Time               `json:"now"`
	Timezone  string                  `json:"timezone"`
	Username  string                  `json:"username"`
	Cards     map[string]cache.Status `json:"cards"`
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	out := health{
		Version:   s.version,
		StartedAt: s.startedAt,
		Now:       time.Now().UTC(),
		Timezone:  s.cfg.Timezone,
		Username:  s.cfg.Username,
		Cards:     map[string]cache.Status{},
	}

	ok := true
	if s.cfg.Languages.Enabled {
		st := s.store.Status(CardLangs)
		out.Cards[CardLangs] = st
		ok = ok && st.Healthy
	}
	if s.cfg.Streak.Enabled {
		st := s.store.Status(CardStreak)
		out.Cards[CardStreak] = st
		ok = ok && st.Healthy
	}

	out.Status = "ok"
	code := http.StatusOK
	if !ok {
		out.Status = "unavailable"
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var b strings.Builder
	b.WriteString("gitdisplay\n\n")
	if s.cfg.Languages.Enabled {
		b.WriteString("  /top-langs.svg\n")
	}
	if s.cfg.Streak.Enabled {
		b.WriteString("  /streak.svg\n")
	}
	b.WriteString("  /healthz\n")
	_, _ = w.Write([]byte(b.String()))
}

func securityHeaders(next http.Handler, hsts string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if hsts != "" {
			w.Header().Set("Strict-Transport-Security", hsts)
		}
		next.ServeHTTP(w, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		field := strings.TrimSpace(v)
		name, params, _ := strings.Cut(field, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// "gzip;q=0" means the client does not want it.
		if strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") &&
			!strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0.") {
			return false
		}
		return true
	}
	return false
}

// gzipETag distinguishes the compressed representation from the identity one,
// so a cache holding one never answers a revalidation for the other.
func gzipETag(etag string) string {
	if strings.HasSuffix(etag, `"`) && len(etag) > 1 {
		return etag[:len(etag)-1] + `-gz"`
	}
	return etag + "-gz"
}

func matchesETag(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		// Tolerate a weak validator prefix.
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
