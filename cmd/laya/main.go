// Command laya runs the Laya log classification service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // time zones for log file analysis, even on minimal images

	"github.com/vipinmohan22/layalogs/internal/config"
	"github.com/vipinmohan22/layalogs/internal/engine"
	"github.com/vipinmohan22/layalogs/internal/events"
	"github.com/vipinmohan22/layalogs/internal/importer"
	"github.com/vipinmohan22/layalogs/internal/model"
	"github.com/vipinmohan22/layalogs/internal/notify"
	"github.com/vipinmohan22/layalogs/internal/pipeline"
	"github.com/vipinmohan22/layalogs/internal/poller"
	"github.com/vipinmohan22/layalogs/internal/server"
	"github.com/vipinmohan22/layalogs/web"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "analyze" {
		os.Exit(runAnalyze(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "laya:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil && v > 0 {
		return v
	}
	return def
}

func newLogger() *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(env("LAYA_LOG_LEVEL", "info"))); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if env("LAYA_LOG_FORMAT", "json") == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

func run() error {
	log := newLogger()
	listen := env("LAYA_LISTEN", ":8080")
	dataDir := env("LAYA_DATA_DIR", "./data")
	publicURL := env("LAYA_PUBLIC_URL", "")

	token := env("LAYA_ADMIN_TOKEN", "")
	if token == "" {
		token = model.NewToken()
		log.Warn("LAYA_ADMIN_TOKEN is not set; generated a token for this run only. Set LAYA_ADMIN_TOKEN (e.g. from a Nomad Variable) to keep it stable.", "admin_token", token)
	} else if len(token) < 16 {
		log.Warn("LAYA_ADMIN_TOKEN is shorter than 16 characters; use a long random value")
	}

	cfg, err := config.Open(filepath.Join(dataDir, "config.json"), env("LAYA_CONFIG_SEED", ""))
	if err != nil {
		return err
	}
	for _, w := range config.FileRefWarnings(cfg.Get()) {
		log.Warn("config refers to a file Laya may not read", "detail", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := events.NewStore(envInt("LAYA_EVENT_BUFFER", 5000))
	dispatcher := notify.NewDispatcher(log)
	laya := engine.New(log)
	pipe := pipeline.New(envInt("LAYA_QUEUE_SIZE", 10000), store, dispatcher, laya)
	pollers := poller.NewManager(ctx, log, pipe.SubmitWait)

	cfg.Subscribe(func(c *config.Config) {
		if c.Settings.PublicURL == "" {
			c.Settings.PublicURL = strings.TrimRight(publicURL, "/")
		}
		pipe.Configure(c)
		dispatcher.Configure(c)
		laya.Configure(c)
		pollers.Configure(c)
		log.Info("configuration applied", "version", c.Version, "sources", len(c.Sources), "rules", len(c.Rules), "notifiers", len(c.Notifiers), "routes", len(c.Routes))
	})

	// The pipeline and dispatcher outlive the signal: on shutdown they
	// first finish what was accepted (see below).
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	done := make(chan struct{}, 2)
	go func() { dispatcher.Run(runCtx, envInt("LAYA_NOTIFY_WORKERS", 4)); done <- struct{}{} }()
	go func() {
		pipe.Run(runCtx, envInt("LAYA_WORKERS", 4), envInt("LAYA_ENGINE_WORKERS", 16))
		done <- struct{}{}
	}()

	importMemory := int64(envInt("LAYA_IMPORT_MEMORY_MB", 64)) << 20
	imports := importer.NewRegistry(importer.RegistryConfig{
		Concurrency:   1,
		Limits:        importer.Limits{Events: importer.DefaultLimits.Events, Bytes: min(importer.DefaultLimits.Bytes, importMemory)},
		RetainedBytes: importMemory,
	})

	srv := server.New(server.Options{
		Version:        version,
		AdminToken:     token,
		PublicURL:      publicURL,
		MaxIngestBytes: int64(envInt("LAYA_MAX_INGEST_BYTES", 10<<20)),
		MaxImportBytes: int64(envInt("LAYA_MAX_IMPORT_BYTES", 100<<20)),
		UI:             web.FS(),
	}, log, cfg, store, pipe, dispatcher, laya, pollers, imports)

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      150 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("laya listening", "addr", listen, "data_dir", dataDir, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	stopping := time.Now()
	// Stop ingest (pollers stopped with ctx), then finish every accepted
	// event, engine requests cancelled, and deliver its alerts. Requests
	// and events have their own budgets, so a slow upload can't use up the
	// drain's; notifications get what is left of 26 s, and stopping 2 s
	// more, within Nomad's kill_timeout (30 s).
	step := func(d time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), d)
	}
	httpCtx, cancel := step(8 * time.Second)
	if err := httpSrv.Shutdown(httpCtx); err != nil {
		_ = httpSrv.Close() // in-flight uploads end as interrupted
	}
	cancel()
	drainCtx, cancel := step(8 * time.Second)
	if err := pipe.Drain(drainCtx); err != nil {
		log.Warn("events still queued at shutdown", "queued", pipe.QueueDepth()+pipe.EngineQueueDepth())
	}
	cancel()
	notifyCtx, cancel := context.WithDeadline(context.Background(), stopping.Add(26*time.Second))
	if err := dispatcher.Drain(notifyCtx); err != nil {
		log.Warn("notifications still queued at shutdown", "queued", dispatcher.QueueDepth())
	}
	cancel()
	shutdownCtx, cancel := step(2 * time.Second)
	defer cancel()
	laya.Close()
	stopRun()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-shutdownCtx.Done():
			return nil
		}
	}
	return nil
}
