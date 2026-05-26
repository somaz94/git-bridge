package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"git-bridge/internal/config"
	"git-bridge/internal/consumer"
	"git-bridge/internal/mirror"
	"git-bridge/internal/notify"
	"git-bridge/internal/server"
	"git-bridge/internal/version"
)

const defaultConfigPath = "/etc/git-bridge/config.yaml"

func main() {
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

	// Init mirror service
	mirrorSvc, err := mirror.New(cfg, notifier)
	if err != nil {
		slog.Error("failed to init mirror service", "error", err)
		os.Exit(1)
	}

	// Context with graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Init webhook consumer
	webhook := consumer.NewWebhook(
		ctx,
		mirrorSvc,
		cfg.Webhook.GitLabSecret,
		cfg.Webhook.GitHubSecret,
	)

	// Init retry consumer (handler returns 404 when token is unset).
	retry := consumer.NewRetry(ctx, mirrorSvc, cfg.Retry.APIToken)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup

	// Start HTTP server (health + webhook + retry endpoints)
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.RunServer(ctx, cfg.Server.Port, webhook, retry)
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
				c.Start(ctx)
			}(cc)
		}
	} else {
		slog.Info("SQS consumer disabled (no consumers configured)")
	}

	// Wait for shutdown signal
	sig := <-sigCh
	slog.Info("shutdown signal received", "signal", sig)
	cancel()
	wg.Wait()
	slog.Info("git-bridge stopped")
}
