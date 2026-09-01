package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg   *Config
	cache *Cache
	fetch *Fetcher
}

func NewServer(cfg *Config, cache *Cache, fetch *Fetcher) *Server {
	return &Server{cfg: cfg, cache: cache, fetch: fetch}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/repos.gz", s.handleReposMaster)
	mux.HandleFunc("/find", s.handleFind)
	mux.HandleFunc("/u/", s.handleUpstream)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

// publicBase is the URL peers reach this proxy on. It is what gets baked
// into rewritten repos.gz and /find payloads, so it has to be correct from
// the *client's* point of view, not the server's.
func (s *Server) publicBase(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	return scheme + "://" + host
}

// proxify turns an absolute upstream URL into one that points back here.
func (s *Server) proxify(base, raw string) string {
	u := strings.TrimPrefix(raw, "https://")
	u = strings.TrimPrefix(u, "http://")
	return base + "/u/" + u
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	hits, miss := s.cache.Stats()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "cache_hits "+strconv.FormatInt(hits, 10)+"\n")
	io.WriteString(w, "cache_misses "+strconv.FormatInt(miss, 10)+"\n")
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	base := s.publicBase(r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "BAR-proxy\n\nPoint pr-downloader at this host:\n\n")
	io.WriteString(w, "  PRD_RAPID_REPO_MASTER="+base+"/repos.gz\n")
	io.WriteString(w, "  PRD_HTTP_SEARCH_URL="+base+"/find\n")
}

// handleUpstream is the transparent half: /u/<host>/<path> fetches
// https://<host>/<path>, cached where the object is immutable.
func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/u/")
	if rest == "" {
		http.Error(w, "missing upstream host", http.StatusBadRequest)
		return
	}

	host, path, _ := strings.Cut(rest, "/")
	if host == "" {
		http.Error(w, "missing upstream host", http.StatusBadRequest)
		return
	}
	if !s.cfg.hostAllowed(host) {
		// Refusing unknown hosts is what keeps this from being an open
		// relay that anyone on the internet can bounce traffic through.
		http.Error(w, "upstream host not allowed: "+host, http.StatusForbidden)
		return
	}

	target := "https://" + host + "/" + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	// The rapid streamer speaks POST and its responses are request-specific,
	// so they are streamed straight through without touching the cache.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.passthrough(w, r, target)
		return
	}
	// A ranged miss would need random access into a file we are still
	// filling sequentially; pass those through instead.
	if r.Header.Get("Range") != "" {
		if e, file := s.cache.Get(target); e != nil {
			defer file.Close()
			s.serveCached(w, r, e, file)
			return
		}
		s.passthrough(w, r, target)
		return
	}

	s.serve(w, r, target)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, target string) {
	if e, file := s.cache.Get(target); e != nil {
		defer file.Close()
		s.serveCached(w, r, e, file)
		return
	}

	entry, body, err := s.fetch.Open(r.Context(), target, r.Header)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		var se *upstreamStatusError
		if errors.As(err, &se) {
			// Upstream's answer, not our failure. Forward it verbatim so a
			// 404 stays a 404 instead of becoming an empty 200.
			if s.cfg.Verbose {
				log.Printf("proxy: %s: %s", target, se.status)
			}
			http.Error(w, se.status, se.code)
			return
		}
		log.Printf("proxy: %s: %v", target, err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer body.Close()

	h := w.Header()
	if entry.ContentType != "" {
		h.Set("Content-Type", entry.ContentType)
	}
	if entry.Size > 0 {
		h.Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	}
	if entry.Immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	h.Set("X-BAR-Cache", "MISS")
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, body); err != nil && s.cfg.Verbose {
		log.Printf("proxy: client dropped %s: %v", target, err)
	}
}

func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, e *Entry, file *os.File) {
	h := w.Header()
	if e.ContentType != "" {
		h.Set("Content-Type", e.ContentType)
	}
	if e.ETag != "" {
		h.Set("ETag", e.ETag)
	}
	if e.Immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	h.Set("X-BAR-Cache", "HIT")
	http.ServeContent(w, r, "", e.FetchedAt, file)
}

// passthrough is a dumb streaming relay for anything not worth caching.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, target string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "bad upstream request", http.StatusBadRequest)
		return
	}
	for k, vs := range r.Header {
		if hopByHop[strings.ToLower(k)] && strings.ToLower(k) != "range" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", "BAR-proxy/1.0 (+pr-downloader content proxy)")

	resp, err := s.fetch.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		log.Printf("proxy: passthrough %s: %v", target, err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if hopByHop[strings.ToLower(k)] && strings.ToLower(k) != "range" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-BAR-Cache", "BYPASS")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func logRequests(next http.Handler, verbose bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if verbose {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
