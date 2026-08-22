package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("barproxy: ")

	cfg := loadConfig()

	cache, err := NewCache(cfg.CacheDir, cfg.CacheMaxBytes, cfg.MutableTTL)
	if err != nil {
		log.Fatalf("cache: %v", err)
	}
	go cache.SweepLoop(30 * time.Minute)

	fetcher := NewFetcher(cfg, cache)
	srv := NewServer(cfg, cache, fetcher)

	handler := logRequests(srv.Routes(), cfg.Verbose)

	httpSrv := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
		// No WriteTimeout: a 2 GB map served to a slow peer would trip it.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("listening on %s (cache=%s parts=%d)", cfg.Addr, cfg.CacheDir, cfg.Parts)
		log.Printf("upstream master: %s", cfg.UpstreamMaster)
		log.Printf("upstream find:   %s", cfg.UpstreamFind)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
}
