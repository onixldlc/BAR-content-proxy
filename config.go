package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is resolved once at startup from the environment.
type Config struct {
	Addr string

	CacheDir      string
	CacheMaxBytes int64
	MutableTTL    time.Duration

	// PublicURL overrides the base URL written into rewritten payloads.
	// Empty means "derive it from the incoming request", which is what you
	// want unless the proxy sits behind another reverse proxy that mangles
	// Host.
	PublicURL string

	UpstreamMaster string
	UpstreamFind   string

	// AllowedHosts is the exact-match set; AllowedSuffixes covers whole
	// domains. Together they stop this from being an open relay.
	AllowedHosts    map[string]bool
	AllowedSuffixes []string

	Parts       int
	PartMinSize int64

	Verbose bool
}

const (
	defaultMaster = "https://repos-cdn.beyondallreason.dev/repos.gz"
	defaultFind   = "https://files-cdn.beyondallreason.dev/find"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// Warn and carry on rather than exiting. Under a restart policy a
		// fatal config error becomes a crash-loop, which is a far worse
		// failure than running with the default.
		log.Printf("config: %s=%q is not a number (%v); using default %d", key, v, err, def)
		return def
	}
	return n
}

func envDur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("config: %s=%q is not a duration (%v); using default %s", key, v, err, def)
		return def
	}
	return d
}

func loadConfig() *Config {
	c := &Config{
		Addr:           env("BARPROXY_ADDR", ":8080"),
		CacheDir:       env("BARPROXY_CACHE_DIR", "./cache"),
		CacheMaxBytes:  envInt("BARPROXY_CACHE_MAX_BYTES", 0),
		MutableTTL:     envDur("BARPROXY_MUTABLE_TTL", 5*time.Minute),
		PublicURL:      strings.TrimRight(os.Getenv("BARPROXY_PUBLIC_URL"), "/"),
		UpstreamMaster: env("BARPROXY_UPSTREAM_MASTER", defaultMaster),
		UpstreamFind:   env("BARPROXY_UPSTREAM_FIND", defaultFind),
		Parts:          int(envInt("BARPROXY_PARTS", 4)),
		PartMinSize:    envInt("BARPROXY_PART_MIN_SIZE", 8<<20),
		Verbose:        os.Getenv("BARPROXY_VERBOSE") == "1",
		AllowedHosts:   map[string]bool{},
		AllowedSuffixes: []string{
			".beyondallreason.dev",
		},
	}

	for _, h := range []string{
		"repos.springrts.com",
		"springfiles.springrts.com",
		"files.springrts.com",
	} {
		c.AllowedHosts[h] = true
	}

	// BARPROXY_ALLOW adds hosts; a leading dot means "and all subdomains".
	for _, h := range strings.Split(os.Getenv("BARPROXY_ALLOW"), ",") {
		h = strings.TrimSpace(strings.ToLower(h))
		if h == "" {
			continue
		}
		if strings.HasPrefix(h, ".") {
			c.AllowedSuffixes = append(c.AllowedSuffixes, h)
		} else {
			c.AllowedHosts[h] = true
		}
	}

	if c.Parts < 1 {
		c.Parts = 1
	}
	return c
}

func (c *Config) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if c.AllowedHosts[host] {
		return true
	}
	for _, sfx := range c.AllowedSuffixes {
		if strings.HasSuffix(host, sfx) {
			return true
		}
	}
	return false
}
