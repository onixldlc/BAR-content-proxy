package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The rapid protocol only ever states absolute URLs in one place: the
// master repos.gz. Every other path (versions.gz, packages/<md5>.sdp,
// pool/<xx>/<hash>.gz, streamer.cgi) is resolved relative to the repo base
// URL found there. Rewrite that one file and the whole download tree
// follows the proxy on its own.
//
// Format is CSV, one repo per line:
//
//	byar,https://repos-cdn.beyondallreason.dev/byar,,

type masterCache struct {
	mu      sync.Mutex
	body    []byte
	base    string
	fetched time.Time
}

var master masterCache

func (s *Server) handleReposMaster(w http.ResponseWriter, r *http.Request) {
	base := s.publicBase(r)

	master.mu.Lock()
	fresh := master.body != nil &&
		master.base == base &&
		time.Since(master.fetched) < s.cfg.MutableTTL
	if fresh {
		body := master.body
		master.mu.Unlock()
		writeGz(w, body, "HIT")
		return
	}
	master.mu.Unlock()

	raw, err := s.fetchAll(r, s.cfg.UpstreamMaster)
	if err != nil {
		log.Printf("rapid: master fetch: %v", err)
		http.Error(w, "upstream master unavailable", http.StatusBadGateway)
		return
	}

	plain, err := gunzip(raw)
	if err != nil {
		log.Printf("rapid: master gunzip: %v", err)
		http.Error(w, "malformed upstream master", http.StatusBadGateway)
		return
	}

	rewritten := s.rewriteMaster(plain, base)
	packed, err := gzipBytes(rewritten)
	if err != nil {
		http.Error(w, "gzip failed", http.StatusInternalServerError)
		return
	}

	master.mu.Lock()
	master.body = packed
	master.base = base
	master.fetched = time.Now()
	master.mu.Unlock()

	writeGz(w, packed, "MISS")
}

func (s *Server) rewriteMaster(plain []byte, base string) []byte {
	var out bytes.Buffer
	for _, line := range strings.Split(string(plain), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "http") {
			fields[1] = s.proxify(base, fields[1])
		}
		out.WriteString(strings.Join(fields, ","))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// handleFind proxies the springfiles-style search endpoint, rewriting the
// mirror list so the client downloads the actual archive through us too.
func (s *Server) handleFind(w http.ResponseWriter, r *http.Request) {
	target := s.cfg.UpstreamFind
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	raw, err := s.fetchAll(r, target)
	if err != nil {
		log.Printf("rapid: find fetch: %v", err)
		http.Error(w, "upstream search unavailable", http.StatusBadGateway)
		return
	}

	base := s.publicBase(r)
	rewritten, err := s.rewriteFind(raw, base)
	if err != nil {
		// Not JSON we recognise; hand back what upstream said rather than
		// breaking the client outright.
		log.Printf("rapid: find rewrite: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-BAR-Cache", "BYPASS")
	w.Write(rewritten)
}

func (s *Server) rewriteFind(raw []byte, base string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber keeps file sizes as integers instead of turning them into
	// float64 and re-encoding them in scientific notation.
	dec.UseNumber()

	var results []map[string]any
	if err := dec.Decode(&results); err != nil {
		return nil, err
	}

	for _, item := range results {
		mirrors, ok := item["mirrors"].([]any)
		if !ok {
			continue
		}
		for i, m := range mirrors {
			u, ok := m.(string)
			if !ok || !strings.HasPrefix(u, "http") {
				continue
			}
			if !s.cfg.hostAllowed(hostOf(u)) {
				continue
			}
			mirrors[i] = s.proxify(base, u)
		}
		item["mirrors"] = mirrors
	}
	return json.Marshal(results)
}

func hostOf(raw string) string {
	u := strings.TrimPrefix(raw, "https://")
	u = strings.TrimPrefix(u, "http://")
	host, _, _ := strings.Cut(u, "/")
	return host
}

// fetchAll pulls a small control file fully into memory.
func (s *Server) fetchAll(r *http.Request, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	copyUpstreamHeaders(req.Header, r.Header)

	resp, err := s.fetch.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{target: target, status: resp.Status}
	}
	// 32 MiB ceiling: these are control files, not archives.
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

type httpError struct {
	target string
	status string
}

func (e *httpError) Error() string { return "upstream " + e.target + ": " + e.status }

func gunzip(raw []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 64<<20))
}

func gzipBytes(plain []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeGz(w http.ResponseWriter, body []byte, cacheState string) {
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-cache")
	h.Set("X-BAR-Cache", cacheState)
	w.Write(body)
}
