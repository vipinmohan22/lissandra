// Command lissandra runs the Lissandra log classification service.
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

	"github.com/vipinmohan22/lissandra/internal/auth"
	"github.com/vipinmohan22/lissandra/internal/config"
	"github.com/vipinmohan22/lissandra/internal/engine"
	"github.com/vipinmohan22/lissandra/internal/events"
	"github.com/vipinmohan22/lissandra/internal/importer"
	"github.com/vipinmohan22/lissandra/internal/learn"
	"github.com/vipinmohan22/lissandra/internal/notify"
	"github.com/vipinmohan22/lissandra/internal/pipeline"
	"github.com/vipinmohan22/lissandra/internal/poller"
	"github.com/vipinmohan22/lissandra/internal/server"
	"github.com/vipinmohan22/lissandra/web"
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
		fmt.Fprintln(os.Stderr, "lissandra:", err)
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
	if err := lvl.UnmarshalText([]byte(env("LISSANDRA_LOG_LEVEL", "info"))); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if env("LISSANDRA_LOG_FORMAT", "json") == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// openAdmin picks the admin token (LISSANDRA_ADMIN_TOKEN, else one set in
// Lissandra, else one generated for this run) and logs where it comes from.
// Only a generated token is logged, for the operator to sign in with.
func openAdmin(log *slog.Logger, path, envToken string) (*auth.Store, error) {
	admin, generated, err := auth.Open(path, envToken, log)
	if err != nil {
		return nil, err
	}
	switch st := admin.Status(); st.Source {
	case auth.SourceGenerated:
		log.Warn("LISSANDRA_ADMIN_TOKEN is not set and no admin token was set in Lissandra; generated a token for this run only. "+
			"Sign in with it to run the setup wizard, which sets a permanent one, or set LISSANDRA_ADMIN_TOKEN.", "admin_token", generated)
	case auth.SourceStored:
		log.Info("admin token: set in Lissandra", "set_at", *st.SetAt, "file", path)
	default:
		if len(envToken) < 16 {
			log.Warn("LISSANDRA_ADMIN_TOKEN is shorter than 16 characters; use a long random value")
		}
		if st.StoredIgnored {
			log.Warn("a token set in Lissandra is ignored while LISSANDRA_ADMIN_TOKEN is set; delete it in Settings → Access", "file", path)
		} else {
			log.Info("admin token: set by LISSANDRA_ADMIN_TOKEN")
		}
	}
	return admin, nil
}

func run() error {
	log := newLogger()
	listen := env("LISSANDRA_LISTEN", ":8080")
	dataDir := env("LISSANDRA_DATA_DIR", "./data")
	publicURL := env("LISSANDRA_PUBLIC_URL", "")

	admin, err := openAdmin(log, filepath.Join(dataDir, "admin-token.json"), env("LISSANDRA_ADMIN_TOKEN", ""))
	if err != nil {
		return err
	}

	cfg, err := config.Open(filepath.Join(dataDir, "config.json"), env("LISSANDRA_CONFIG_SEED", ""))
	if err != nil {
		return err
	}
	// Learned rules saved under an older message pattern: rewrite them and
	// merge the ones that now coincide, once, before anything uses them.
	if rw, rm := learn.MigratePatterns(cfg.Get()); rw+rm > 0 {
		if err := cfg.Update(func(c *config.Config) error { learn.MigratePatterns(c); return nil }); err != nil {
			log.Warn("updating learned rules to the new message patterns failed; they still match", "err", err)
		} else {
			log.Info("updated learned rules to the new message patterns", "rewritten", rw, "merged_away", rm)
		}
	}
	if missing, changed := config.DefaultRuleChanges(cfg.Get()); len(missing)+len(changed) > 0 {
		log.Info("this version's default rules differ from the configured ones; review them in Rules → Default rules", "missing", len(missing), "changed", len(changed))
	}
	for _, w := range config.FileRefWarnings(cfg.Get()) {
		log.Warn("config refers to a file Lissandra may not read", "detail", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := events.NewStore(envInt("LISSANDRA_EVENT_BUFFER", 5000))
	// Alert state (windows, pending digests, per-pattern baselines) is
	// loaded here and saved every minute and at shutdown.
	dispatcher := notify.NewDispatcherWith(log, notify.Options{StatePath: filepath.Join(dataDir, "alerting.json")})
	eng := engine.New(log)
	pipe := pipeline.New(envInt("LISSANDRA_QUEUE_SIZE", 10000), store, dispatcher, eng)
	learner := learn.New(log, cfg.Update)
	pipe.Learn = learner.Observe
	pollers := poller.NewManager(ctx, log, pipe.SubmitWait)

	cfg.Subscribe(func(c *config.Config) {
		if c.Settings.PublicURL == "" {
			c.Settings.PublicURL = strings.TrimRight(publicURL, "/")
		}
		pipe.Configure(c)
		dispatcher.Configure(c)
		eng.Configure(c)
		learner.Configure(c)
		pollers.Configure(c)
		log.Info("configuration applied", "version", c.Version, "sources", len(c.Sources), "rules", len(c.Rules), "notifiers", len(c.Notifiers), "routes", len(c.Routes))
	})

	// The pipeline and dispatcher outlive the signal: on shutdown they
	// first finish what was accepted (see below).
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	done := make(chan struct{}, 2)
	go func() { dispatcher.Run(runCtx, envInt("LISSANDRA_NOTIFY_WORKERS", 4)); done <- struct{}{} }()
	// Learned rules are written until the signal: rules proposed during the
	// drain are learned again when the engine next answers their pattern.
	go learner.Run(ctx)
	go func() {
		pipe.Run(runCtx, envInt("LISSANDRA_WORKERS", 4), envInt("LISSANDRA_ENGINE_WORKERS", 16))
		done <- struct{}{}
	}()

	importMemory := int64(envInt("LISSANDRA_IMPORT_MEMORY_MB", 64)) << 20
	imports := importer.NewRegistry(importer.RegistryConfig{
		Concurrency:   1,
		Limits:        importer.Limits{Events: importer.DefaultLimits.Events, Bytes: min(importer.DefaultLimits.Bytes, importMemory)},
		RetainedBytes: importMemory,
	})

	srv := server.New(server.Options{
		Version:        version,
		Auth:           admin,
		OnNomad:        os.Getenv("NOMAD_ALLOC_ID") != "",
		PublicURL:      publicURL,
		MaxIngestBytes: int64(envInt("LISSANDRA_MAX_INGEST_BYTES", 10<<20)),
		MaxImportBytes: int64(envInt("LISSANDRA_MAX_IMPORT_BYTES", 100<<20)),
		UI:             web.FS(),
	}, log, cfg, store, pipe, dispatcher, eng, learner, pollers, imports)

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
		log.Info("lissandra listening", "addr", listen, "data_dir", dataDir, "version", version)
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
	// drain's; notifications (digests already due, then saving alert
	// state) get what is left of 26 s, and stopping 2 s more, within
	// Nomad's kill_timeout (30 s).
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
	eng.Close()
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
