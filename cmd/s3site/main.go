// s3site serves static websites from tar.gz archives stored in S3.
//
// Each archive in the configured bucket is named after the hostname it
// serves: foo.example.com.tar.gz -> serves requests for foo.example.com.
//
// The server polls S3 for changes and hot-reloads sites when archives
// are added, updated, or removed. No restart required.
//
// Usage:
//
//	s3site \
//	  -bucket static-assets \
//	  -region us-east-1 \
//	  -listen :8080 \
//	  -poll 30s
//
// Environment variables (override flags):
//
//	AWS_S3_BUCKET, AWS_S3_REGION, AWS_S3_ACCESS_KEY, AWS_S3_SECRET_KEY,
//	AWS_S3_ENDPOINT (for MinIO/custom S3), S3SITE_LISTEN, S3SITE_POLL
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/rhnvrm/s3site/pkg/s3site"
	"github.com/rhnvrm/simples3"
)

func main() {
	var (
		bucket   = flag.String("bucket", envOr("AWS_S3_BUCKET", ""), "S3 bucket name")
		region   = flag.String("region", envOr("AWS_S3_REGION", "us-east-1"), "AWS region")
		ak       = flag.String("access-key", envOr("AWS_S3_ACCESS_KEY", ""), "AWS access key")
		sk       = flag.String("secret-key", envOr("AWS_S3_SECRET_KEY", ""), "AWS secret key")
		endpoint = flag.String("endpoint", envOr("AWS_S3_ENDPOINT", ""), "Custom S3 endpoint (MinIO)")
		prefix   = flag.String("prefix", envOr("S3SITE_PREFIX", ""), "S3 key prefix (optional)")
		listen   = flag.String("listen", envOr("S3SITE_LISTEN", ":8080"), "HTTP listen address")
		poll     = flag.Duration("poll", envDurOr("S3SITE_POLL", 30*time.Second), "Poll interval")
		admHost  = flag.String("admin-host", envOr("S3SITE_ADMIN_HOST", ""), "Hostname for admin UI (e.g. s3site-admin.example.com)")
		storage  = flag.String("storage", envOr("S3SITE_STORAGE", "memory"), "Storage mode: memory or disk")
		dataDir  = flag.String("data-dir", envOr("S3SITE_DATA_DIR", ""), "Directory for disk storage (default: $TMPDIR/s3site-data)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if *bucket == "" {
		logger.Error("bucket is required (-bucket or AWS_S3_BUCKET)")
		os.Exit(1)
	}

	// Create S3 client.
	s3Client := simples3.New(*region, *ak, *sk)
	if *endpoint != "" {
		s3Client.SetEndpoint(*endpoint)
	}

	// Parse storage mode.
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

	// Create site manager.
	sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
		S3:       s3Client,
		Bucket:   *bucket,
		Prefix:   *prefix,
		Interval: *poll,
		Logger:   logger,
		Storage:  storageMode,
		DataDir:  *dataDir,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Start polling in background.
	go func() {
		if err := sm.Start(ctx); err != nil && err != context.Canceled {
			logger.Error("site manager error", "error", err)
		}
	}()

	// Start HTTP server.
	handler := s3site.Handler(sm, logger, *admHost)

	srv := &http.Server{
		Addr:    *listen,
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	logger.Info("s3site: listening",
		"addr", *listen,
		"bucket", *bucket,
		"prefix", *prefix,
		"poll", *poll,
		"storage", *storage,
		"admin_host", *admHost,
	)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("http server error", "error", err)
		os.Exit(1)
	}
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
