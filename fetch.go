package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Fetcher pulls upstream objects into the cache. One in-flight download per
// URL is shared by every client that asks for it, so twenty peers starting
// the same map at once cost one upstream transfer.
type Fetcher struct {
	cfg    *Config
	cache  *Cache
	client *http.Client

	mu       sync.Mutex
	inflight map[string]*download
}

type download struct {
	url   string
	entry *Entry
	pf    *partialFile

	ready chan struct{} // closed once entry/pf are populated, or err is set
	err   error
}

func newHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	// No client-wide Timeout: pool files and map archives are large and a
	// blanket deadline would kill legitimate slow transfers mid-stream.
	return &http.Client{Transport: tr}
}

func NewFetcher(cfg *Config, cache *Cache) *Fetcher {
	return &Fetcher{
		cfg:      cfg,
		cache:    cache,
		client:   newHTTPClient(),
		inflight: map[string]*download{},
	}
}

// immutableURL reports whether an object can be cached forever. Rapid pool
// entries, .sdp packages and files-cdn /file/<md5>/ paths are all
// content-addressed, so their bytes can never change.
func immutableURL(u string) bool {
	switch {
	case strings.Contains(u, "/pool/"):
		return true
	case strings.Contains(u, "/packages/"):
		return true
	case strings.Contains(u, "/file/"):
		return true
	}
	return false
}

// Open returns a reader for url, serving from cache when possible. The
// caller must Close the reader.
func (f *Fetcher) Open(ctx context.Context, url string, hdr http.Header) (*Entry, io.ReadCloser, error) {
	if e, file := f.cache.Get(url); e != nil {
		return e, file, nil
	}
	f.cache.MarkMiss()

	f.mu.Lock()
	d, joined := f.inflight[url]
	if !joined {
		d = &download{url: url, ready: make(chan struct{})}
		f.inflight[url] = d
		go f.run(d, hdr)
	}
	f.mu.Unlock()

	select {
	case <-d.ready:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	if d.err != nil {
		return nil, nil, d.err
	}
	return d.entry, d.pf.NewReader(), nil
}

func (f *Fetcher) forget(url string) {
	f.mu.Lock()
	delete(f.inflight, url)
	f.mu.Unlock()
}

// upstreamStatusError is a definitive non-200 answer from upstream: the
// object is not there, and no fallback will conjure it. Distinct from a
// transport error, which is worth retrying a different way.
type upstreamStatusError struct {
	url    string
	code   int
	status string
}

func (e *upstreamStatusError) Error() string {
	return "upstream " + e.url + ": " + e.status
}

// probe asks upstream for size and range support without pulling the body.
func (f *Fetcher) probe(url string, hdr http.Header) (size int64, ranges bool, ctype, etag string, err error) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return 0, false, "", "", err
	}
	copyUpstreamHeaders(req.Header, hdr)

	resp, err := f.client.Do(req)
	if err != nil {
		return 0, false, "", "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return 0, false, "", "", &upstreamStatusError{url: url, code: resp.StatusCode, status: resp.Status}
	}
	ranges = strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
	return resp.ContentLength, ranges, resp.Header.Get("Content-Type"), resp.Header.Get("ETag"), nil
}

func (f *Fetcher) run(d *download, hdr http.Header) {
	// Deferred LIFO: forget() first, so no new client can join, and only
	// then release the downloader's reference on the file.
	defer func() {
		if d.pf != nil {
			d.pf.release()
		}
	}()
	defer f.forget(d.url)

	fail := func(err error) {
		d.err = err
		close(d.ready)
	}

	size, ranges, ctype, etag, err := f.probe(d.url, hdr)
	if err != nil {
		// 405/501 means the server dislikes HEAD, not that the object is
		// missing, so a plain GET is still worth trying. Any other definite
		// status (404, 403, 410) is upstream's real answer and has to reach
		// the client as such. Falling through would publish the entry as 200
		// before the body is fetched, so pr-downloader receives a zero-byte
		// 200 and treats a missing pool object as a valid empty file.
		var se *upstreamStatusError
		if errors.As(err, &se) && se.code != http.StatusMethodNotAllowed && se.code != http.StatusNotImplemented {
			fail(err)
			return
		}
		// HEAD is not universally supported; fall back to a plain stream.
		if f.cfg.Verbose {
			log.Printf("fetch: probe failed for %s: %v (falling back to single stream)", d.url, err)
		}
		size, ranges, ctype, etag = -1, false, "", ""
	}

	parts := f.plan(size, ranges)

	tmp, err := f.cache.NewTemp(d.url)
	if err != nil {
		fail(err)
		return
	}
	if size > 0 {
		if err := tmp.Truncate(size); err != nil {
			f.cache.Discard(tmp)
			tmp.Close()
			fail(err)
			return
		}
	}

	sizes := make([]int64, len(parts))
	for i, p := range parts {
		sizes[i] = p.length
	}
	if len(sizes) == 0 {
		sizes = []int64{size}
	}

	pf := newPartialFile(tmp, size, sizes)
	d.pf = pf
	d.entry = &Entry{
		URL:         d.url,
		Status:      http.StatusOK,
		ContentType: ctype,
		Size:        size,
		ETag:        etag,
		FetchedAt:   time.Now(),
		Immutable:   immutableURL(d.url),
	}
	close(d.ready)

	// From here the download is detached from any particular client: if
	// every reader disconnects, the transfer still completes and lands in
	// the cache for the next peer.
	err = f.download(d.url, hdr, parts, pf)
	if err != nil {
		f.cache.Discard(tmp)
		pf.finish(err)
		log.Printf("fetch: %s failed: %v", d.url, err)
		return
	}

	if pf.unknown {
		// Length only became known as the body drained.
		pf.mu.Lock()
		d.entry.Size = pf.prog[0]
		pf.total = d.entry.Size
		pf.mu.Unlock()
	}

	if err := f.cache.Commit(tmp, d.entry); err != nil {
		log.Printf("fetch: commit %s: %v", d.url, err)
	}
	pf.finish(nil)
}

type partRange struct {
	start  int64
	length int64
}

func (f *Fetcher) plan(size int64, ranges bool) []partRange {
	if size <= 0 || !ranges || f.cfg.Parts <= 1 || size < f.cfg.PartMinSize {
		return []partRange{{start: 0, length: size}}
	}
	n := int64(f.cfg.Parts)
	chunk := size / n
	out := make([]partRange, 0, n)
	for i := int64(0); i < n; i++ {
		start := i * chunk
		length := chunk
		if i == n-1 {
			length = size - start
		}
		out = append(out, partRange{start: start, length: length})
	}
	return out
}

func (f *Fetcher) download(url string, hdr http.Header, parts []partRange, pf *partialFile) error {
	if len(parts) == 1 && parts[0].length <= 0 {
		return f.fetchPart(url, hdr, 0, partRange{start: 0, length: -1}, pf, false)
	}
	if len(parts) == 1 {
		return f.fetchPart(url, hdr, 0, parts[0], pf, false)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(parts))
	for i, p := range parts {
		wg.Add(1)
		go func(i int, p partRange) {
			defer wg.Done()
			errs[i] = f.fetchPart(url, hdr, i, p, pf, true)
		}(i, p)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

const partRetries = 3

func (f *Fetcher) fetchPart(url string, hdr http.Header, idx int, p partRange, pf *partialFile, ranged bool) error {
	var lastErr error
	for attempt := 0; attempt < partRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
			pf.reset(idx)
		}
		err := f.fetchPartOnce(url, hdr, idx, p, pf, ranged)
		if err == nil {
			return nil
		}
		lastErr = err
		if f.cfg.Verbose {
			log.Printf("fetch: %s part %d attempt %d: %v", url, idx, attempt+1, err)
		}
	}
	return lastErr
}

func (f *Fetcher) fetchPartOnce(url string, hdr http.Header, idx int, p partRange, pf *partialFile, ranged bool) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	copyUpstreamHeaders(req.Header, hdr)
	if ranged {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", p.start, p.start+p.length-1))
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if ranged && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("upstream ignored Range for %s: %s", url, resp.Status)
	}
	if !ranged && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream GET %s: %s", url, resp.Status)
	}

	w := &partWriter{pf: pf, idx: idx, off: p.start}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return err
	}
	if p.length > 0 && n != p.length {
		return fmt.Errorf("short part %d for %s: got %d want %d", idx, url, n, p.length)
	}
	return nil
}

// hopByHop headers must not be forwarded upstream.
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"host":                true,
	"range":               true,
	"accept-encoding":     true,
	"if-none-match":       true,
	"if-modified-since":   true,
}

func copyUpstreamHeaders(dst, src http.Header) {
	dst.Set("User-Agent", "BAR-proxy/1.0 (+pr-downloader content proxy)")
	if src == nil {
		return
	}
	for k, vs := range src {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
