// s3site serves static websites from tar.gz archives stored in S3.
//
// Each archive in the configured bucket is named after the hostname it
// serves: foo.example.com.tar.gz -> serves requests for foo.example.com.
//
// The server polls S3 for changes and hot-reloads sites when archives
// are added, updated, or removed. No restart required.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rhnvrm/s3site/pkg/s3site"
	"github.com/rhnvrm/simples3"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "refresh":
			if err := runRefresh(os.Args[2:]); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return
				}
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}

	runServe(os.Args[1:])
}

func runServe(args []string) {
	fs := flag.NewFlagSet("s3site", flag.ExitOnError)
	var (
		bucket             = fs.String("bucket", envOr("AWS_S3_BUCKET", ""), "S3 bucket name")
		region             = fs.String("region", envOr("AWS_S3_REGION", "us-east-1"), "AWS region")
		ak                 = fs.String("access-key", envOr("AWS_S3_ACCESS_KEY", ""), "AWS access key")
		sk                 = fs.String("secret-key", envOr("AWS_S3_SECRET_KEY", ""), "AWS secret key")
		endpoint           = fs.String("endpoint", envOr("AWS_S3_ENDPOINT", ""), "Custom S3 endpoint (MinIO)")
		prefix             = fs.String("prefix", envOr("S3SITE_PREFIX", ""), "S3 key prefix (optional)")
		listen             = fs.String("listen", envOr("S3SITE_LISTEN", ":8080"), "HTTP listen address")
		poll               = fs.Duration("poll", envDurOr("S3SITE_POLL", 30*time.Second), "Poll interval")
		admHost            = fs.String("admin-host", envOr("S3SITE_ADMIN_HOST", ""), "Hostname for insecure dev admin UI")
		allowInsecureAdmin = fs.Bool("allow-insecure-admin", envBoolOr("S3SITE_ALLOW_INSECURE_ADMIN", false), "Allow the insecure public admin UI/API")
		storage            = fs.String("storage", envOr("S3SITE_STORAGE", "memory"), "Storage mode: memory or disk")
		dataDir            = fs.String("data-dir", envOr("S3SITE_DATA_DIR", ""), "Directory for disk storage (default: $TMPDIR/s3site-data)")
		sitesConfig        = fs.String("sites-config", envOr("S3SITE_SITES_CONFIG", ""), "Path to hosted-sites JSON config")
		controlSocket      = fs.String("control-socket", envOr("S3SITE_CONTROL_SOCKET", ""), "Unix socket for local control operations")
	)
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if *bucket == "" {
		logger.Error("bucket is required (-bucket or AWS_S3_BUCKET)")
		os.Exit(1)
	}
	if *poll < 0 {
		logger.Error("poll must be >= 0", "poll", *poll)
		os.Exit(1)
	}
	if *admHost != "" && !*allowInsecureAdmin {
		logger.Error("admin host requires -allow-insecure-admin; use the unix control socket for production refreshes")
		os.Exit(1)
	}

	var hostedSites []s3site.HostedSite
	if *sitesConfig != "" {
		var err error
		hostedSites, err = s3site.LoadHostedSites(*sitesConfig, *prefix)
		if err != nil {
			logger.Error("failed to load hosted sites config", "path", *sitesConfig, "error", err)
			os.Exit(1)
		}
	}

	s3Client := simples3.New(*region, *ak, *sk)
	if *endpoint != "" {
		s3Client.SetEndpoint(*endpoint)
	}

	var storageMode s3site.StorageMode
	switch strings.ToLower(*storage) {
	case "memory", "mem", "":
		storageMode = s3site.StorageMemory
	case "disk":
		storageMode = s3site.StorageDisk
	default:
		logger.Error("invalid storage mode, must be 'memory' or 'disk'", "storage", *storage)
		os.Exit(1)
	}

	sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
		S3:          s3Client,
		Bucket:      *bucket,
		Prefix:      *prefix,
		Interval:    *poll,
		Logger:      logger,
		Storage:     storageMode,
		DataDir:     *dataDir,
		HostedSites: hostedSites,
	})
	if err := sm.Validate(); err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	managerErrCh := make(chan error, 1)
	go func() {
		managerErrCh <- sm.Start(ctx)
	}()

	for !sm.Ready() {
		select {
		case err := <-managerErrCh:
			if err != nil && err != context.Canceled {
				logger.Error("site manager startup failed", "error", err)
				os.Exit(1)
			}
			return
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}

	go func() {
		if err := <-managerErrCh; err != nil && err != context.Canceled {
			logger.Error("site manager error", "error", err)
			stop()
		}
	}()

	var controlListener net.Listener
	if *controlSocket != "" {
		var err error
		controlListener, err = s3site.ListenControlSocket(*controlSocket)
		if err != nil {
			logger.Error("control socket startup failed", "socket", *controlSocket, "error", err)
			os.Exit(1)
		}
		go func() {
			if err := s3site.ServeControlServer(ctx, *controlSocket, controlListener, sm, logger); err != nil {
				logger.Error("control server error", "error", err)
				stop()
			}
		}()
	}

	handler := s3site.Handler(sm, logger, *admHost)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	mode := "discovery"
	if sm.HostedMode() {
		mode = "hosted"
	}
	logger.Info("s3site: listening",
		"addr", *listen,
		"bucket", *bucket,
		"prefix", *prefix,
		"poll", *poll,
		"storage", *storage,
		"mode", mode,
		"admin_host", *admHost,
		"control_socket", *controlSocket,
	)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("http server error", "error", err)
		os.Exit(1)
	}
}

func runRefresh(args []string) error {
	fs := flag.NewFlagSet("s3site refresh", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socket := fs.String("socket", envOr("S3SITE_CONTROL_SOCKET", ""), "Unix socket for local control operations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" {
		return fmt.Errorf("refresh requires -socket or S3SITE_CONTROL_SOCKET")
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("refresh requires at least one hostname")
	}

	body, err := json.Marshal(map[string][]string{"hosts": fs.Args()})
	if err != nil {
		return err
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	req, err := http.NewRequest(http.MethodPost, "http://unix/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("refresh failed: %s", strings.TrimSpace(string(respBody)))
	}

	if len(respBody) > 0 {
		fmt.Println(string(respBody))
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDurOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return fallback
}

func envBoolOr(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}
