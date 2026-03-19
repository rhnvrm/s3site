package s3watcher_test

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/rhnvrm/simples3"
	"github.com/rhnvrm/s3site/pkg/s3watcher"
)

func Example() {
	// Create the S3 client.
	s3Client := simples3.New("us-east-1", os.Getenv("AWS_ACCESS_KEY"), os.Getenv("AWS_SECRET_KEY"))

	// Configure the watcher with files to watch.
	w, err := s3watcher.New(s3watcher.Config{
		S3:           s3Client,
		PollInterval: 30 * time.Second,
		Files: []s3watcher.FileEntry{
			{
				Name:   "instruments",
				Bucket: "my-bucket",
				Key:    "data/instruments.csv",
			},
			{
				Name:   "holidays",
				Bucket: "my-bucket",
				Key:    "config/holidays.json",
			},
		},
		Logger: slog.Default(),
	})
	if err != nil {
		panic(err)
	}

	// Register a callback for file changes.
	w.OnUpdate(func(event s3watcher.UpdateEvent) {
		fmt.Printf("file %q updated (%d bytes, etag=%s)\n",
			event.Name, len(event.Data), event.ETag)

		switch event.Name {
		case "instruments":
			// Parse and reload instruments...
		case "holidays":
			// Parse and reload holiday calendar...
		}
	})

	// Start the poll loop in the background.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() {
		if err := w.Start(ctx); err != nil && err != context.Canceled {
			slog.Error("watcher error", "error", err)
		}
	}()

	// Get() lazy-loads on first call - no need to wait for Start().
	data, etag := w.Get("instruments")
	if data != nil {
		fmt.Printf("instruments: %d bytes, etag=%s\n", len(data), etag)
	}

	// Use as an fs.FS - works with anything that accepts the interface.
	fsys := w.FS()
	content, _ := fs.ReadFile(fsys, "holidays")
	fmt.Printf("holidays: %s\n", content)

	// Force re-fetch when you know a file changed (e.g. via webhook).
	_ = w.Refresh("instruments")

	// Evict a file from cache. Next read lazy-loads it fresh.
	w.Clear("instruments")

	// Evict everything.
	w.ClearAll()

	<-ctx.Done()
}
