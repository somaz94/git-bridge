package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"git-bridge/internal/askpass"
	"git-bridge/internal/config"
	"git-bridge/internal/consumer"
	"git-bridge/internal/history"
	"git-bridge/internal/mirror"
	"git-bridge/internal/notify"
	"git-bridge/internal/server"
	"git-bridge/internal/task"
	"git-bridge/internal/version"
)

const defaultConfigPath = "/etc/git-bridge/config.yaml"

func main() {
	// When git asks for credentials it re-executes this binary as the GIT_ASKPASS
	// helper. That call has to print one line of value and nothing else, so it is
	// caught here before the service starts, before the logger is even set up — if
	// the helper mixes a log line into stdout, git reads that first line as the
	// credential. It also has to come before flag.Parse: git passes the prompt as
	// a bare argument, and a prompt that happens to start with "-" would make
	// flag.Parse exit instead.
	if askpass.Serve(os.Args[1:], os.Stdout) {
		return
	}

	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	// JSON structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	slog.Info("starting git-bridge",
		"version", version.Version,
		"commit", version.GitCommit,
		"built", version.BuildDate,
	)

	// Load config
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = defaultConfigPath
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "repos", len(cfg.Repos), "providers", len(cfg.Providers))

	// Init notifier
	var notifier notify.Notifier
	if cfg.Notification.Slack.WebhookURL != "" {
		notifier = notify.NewSlack(cfg.Notification.Slack)
	} else {
		notifier = notify.NewNoop()
	}

	// Init mirror history. A failure here must not keep the service down: the
	// history is an audit trail for the console, not part of mirroring, so we
	// fall back to a no-op sink and carry on with mirroring intact.
	var hist interface {
		history.Recorder
		history.Reader
	} = history.NewNoop()
	if w, err := history.New(filepath.Join(mirror.WorkDir(), history.DirName)); err != nil {
		slog.Error("failed to init mirror history, continuing without it", "error", err)
	} else {
		hist = w
		defer func() { _ = w.Close() }()
	}

	// Init mirror service
	mirrorSvc, err := mirror.New(cfg, notifier, hist)
	if err != nil {
		slog.Error("failed to init mirror service", "error", err)
		os.Exit(1)
	}

	// Shutdown runs on two contexts, and keeping them apart is the whole point.
	//
	// serveCtx is cancelled the moment SIGTERM lands: listeners close and the
	// SQS consumer stops taking new messages. workCtx stays alive through the
	// drain window so a mirror sync already in flight can finish. With a single
	// context the two were the same instant, and a fetch killed mid-run leaves
	// an abandoned pack .keep marker that pins its packfile out of every later
	// repack — exactly the leftover found in the live cache.
	serveCtx, stopServing := context.WithCancel(context.Background())
	defer stopServing()
	workCtx, stopWork := context.WithCancel(context.Background())
	defer stopWork()

	// tasks tracks the syncs the handlers detach, so shutdown has something to
	// wait on. They answer "accepted" and let the sync outlive the request.
	tasks := task.NewGroup(workCtx)

	// Init webhook consumer
	webhook := consumer.NewWebhook(
		tasks,
		mirrorSvc,
		cfg.Webhook.GitLabSecret,
		cfg.Webhook.GitHubSecret,
		cfg.HostResolver(),
	)

	// Init retry consumer (handler returns 404 when token is unset).
	retry := consumer.NewRetry(tasks, mirrorSvc, cfg.Retry.APIToken)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup

	// Start HTTP server (health + webhook endpoints)
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.RunServer(serveCtx, cfg.Server.Port, webhook, retry)
	}()

	// Start the console on its own port. Separate listener, separate mux: the
	// public route only forwards to cfg.Server.Port, so the console is
	// unreachable from outside the cluster without touching that route.
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.RunConsole(serveCtx, cfg.Server.ConsolePort, hist, mirrorSvc, tasks, cfg.Server.APIDocsURL,
			server.WithRestorer(mirrorSvc), server.WithForcer(mirrorSvc))
	}()

	// Start SQS consumers (if configured)
	if len(cfg.Consumers) > 0 {
		for _, cc := range cfg.Consumers {
			wg.Add(1)
			go func(cc config.ConsumerConfig) {
				defer wg.Done()
				c, err := consumer.NewSQS(cc, mirrorSvc)
				if err != nil {
					slog.Error("failed to create SQS consumer", "name", cc.Name, "error", err)
					return
				}
				c.Start(serveCtx, workCtx)
			}(cc)
		}
	} else {
		slog.Info("SQS consumer disabled (no consumers configured)")
	}

	// How long shutdown waits for in-flight syncs. A cap, not a delay: the wait
	// ends the moment the work does, so the common path (median sync ~3s) exits
	// immediately and the value only buys room for the slow tail. Configurable
	// because the right window depends on the repositories being mirrored — see
	// config.MirrorConfig.DrainTimeoutSeconds.
	drainTimeout := time.Duration(cfg.Mirror.DrainTimeoutSeconds) * time.Second

	// Wait for shutdown signal
	sig := <-sigCh
	slog.Info("shutdown signal received", "signal", sig)

	// Close the door before waiting. Listeners stop and the SQS consumer stops
	// taking new messages, so the drain window is spent finishing work rather
	// than racing new arrivals.
	stopServing()

	// wg covers the long-running components, and a consumer only returns once
	// the message it is on is handled; tasks covers the syncs the handlers
	// detached. Both have to settle before it is safe to start killing git.
	if task.WaitAll(drainTimeout, wg.Wait, tasks.Wait) {
		slog.Info("in-flight mirror syncs drained")
	} else {
		// Past this point git commands are killed mid-run. Say so: this is the
		// one moment that can leave a repository needing housekeeping, and a
		// silent exit here is indistinguishable from a clean one.
		slog.Warn("drain window expired, cancelling in-flight mirror syncs",
			"timeout", drainTimeout)
	}
	stopWork()

	slog.Info("git-bridge stopped")
}
