package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	ttscrap "github.com/karilaa-dev/tt-scrap"
	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/httpapi"
	"github.com/karilaa-dev/tt-scrap/internal/identity"
	"github.com/karilaa-dev/tt-scrap/internal/platform"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, "service is not ready")
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func healthcheck() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("invalid port")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + port + "/health/ready")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("not ready")
	}
	return nil
}

func run(logger *slog.Logger) error {
	config, err := core.LoadConfig()
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	switch strings.ToUpper(config.LogLevel) {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARN", "WARNING":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	}
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if config.TempDir != "" {
		if err := os.MkdirAll(config.TempDir, 0700); err != nil {
			return errors.New("cannot create TEMP_DIR")
		}
		info, err := os.Stat(config.TempDir)
		if err != nil || !info.IsDir() {
			return errors.New("TEMP_DIR is not a directory")
		}
	}
	proxies, err := identity.ReadProxies(config)
	if err != nil {
		return err
	}
	shutdownSignal, stopSignal := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignal()
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	var pool *identity.Pool
	var backend core.AppBackend
	var identities core.IdentityPreparer
	ready := func() bool { return true }
	if config.TikTokMethod == "app" {
		pool, err = identity.Open(lifetime, config, proxies)
		if err != nil {
			return err
		}
		defer pool.Close()
		logger.Info("preparing identities before API startup", "status", pool.PreparationStatus())
		if err = pool.Prepare(shutdownSignal); err != nil {
			if shutdownSignal.Err() != nil {
				return nil
			}
			logger.Error("identity preparation incomplete", "status", pool.PreparationStatus())
			return err
		}
		logger.Info("identities prepared", "status", pool.PreparationStatus())
		backend = pool
		identities = pool
		ready = pool.Ready
		proxies = pool.ProxyAddresses()
	}
	assets := core.NewAssets(config.CacheTTL, config.CacheMaxEntries)
	tiktok := platform.NewTikTok(config, assets, backend, proxies)
	defer tiktok.Close()
	instagram := platform.NewInstagram(config, assets)
	defer instagram.Close()
	downloader := platform.NewDownloader(config, backend, proxies)
	defer downloader.Close()
	handler := httpapi.NewHandler(httpapi.Dependencies{Config: config, Assets: assets, TikTok: tiktok, Instagram: instagram, Downloader: downloader, Identities: identities, IsReady: ready, OpenAPI: ttscrap.OpenAPI, OnRequest: func(event httpapi.RequestEvent) {
		logLevel := slog.LevelInfo
		if strings.HasPrefix(event.Path, "/health/") {
			logLevel = slog.LevelDebug
		}
		if event.Status >= 500 {
			logLevel = slog.LevelError
		}
		logger.Log(context.Background(), logLevel, "request completed", "request_id", event.RequestID, "method", event.Method, "path", event.Path, "status", event.Status, "duration_ms", event.DurationMS, "bytes", event.Bytes)
	}})
	server := &http.Server{Addr: net.JoinHostPort(config.Host, strconv.Itoa(config.Port)), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 * 1024, BaseContext: func(net.Listener) context.Context { return lifetime }}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	failures := make(chan error, 1)
	go func() { failures <- server.Serve(listener) }()
	logger.Info("service started", "address", listener.Addr().String(), "tiktok_method", config.TikTokMethod, "proxies", len(proxies))
	if pool != nil {
		logger.Info("identity pool loaded", "stats", pool.Stats())
	}
	select {
	case err = <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-shutdownSignal.Done():
	}
	logger.Info("service stopping")
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = server.Shutdown(shutdown); err != nil {
		cancelLifetime()
		_ = server.Close()
	}
	cancelLifetime()
	return nil
}
