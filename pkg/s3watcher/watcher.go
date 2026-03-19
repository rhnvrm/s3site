// Package s3watcher provides an in-memory file manager that polls S3 objects
// for changes (via HEAD/ETag) and keeps their contents in memory. When an
// object changes, registered callbacks are invoked with the new data.
//
// Files are lazy by default - the first Get() or FS read triggers the
// initial download from S3. Subsequent changes are detected via polling.
package s3watcher

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/rhnvrm/simples3"
)

// FileEntry maps a logical name to an S3 object.
type FileEntry struct {
	// Name is the logical identifier for this file (used in Get and callbacks).
	Name string

	// Bucket is the S3 bucket name.
	Bucket string

	// Key is the S3 object key.
	Key string
}

// UpdateEvent is passed to callbacks when a watched file changes.
type UpdateEvent struct {
	// Name is the logical file name from FileEntry.
	Name string

	// Data is the new file contents.
	Data []byte

	// ETag is the new ETag from S3.
	ETag string

	// LastModified is the Last-Modified header value.
	LastModified string
}

// OnUpdateFunc is a callback invoked when a watched file is updated.
// It is called synchronously during the poll cycle - keep it fast or
// spawn a goroutine for heavy work.
type OnUpdateFunc func(event UpdateEvent)

// Config configures the Watcher.
type Config struct {
	// S3 is the simples3 client used for HEAD and GET requests.
	S3 *simples3.S3

	// PollInterval is how often to check for changes. Default: 1 minute.
	PollInterval time.Duration

	// Files is the list of S3 objects to watch.
	Files []FileEntry

	// Logger is an optional structured logger. If nil, slog.Default() is used.
	Logger *slog.Logger

	// LoadOnStart controls whether all files are fetched eagerly on Start().
	// When true (default), all files are downloaded immediately and marked
	// as accessed so the poll loop watches them all.
	// When false, files are fetched lazily on first Get()/FS read.
	LoadOnStart *bool
}

// fileState holds the in-memory state for a single watched file.
type fileState struct {
	mu       sync.RWMutex
	data     []byte
	etag     string
	accessed bool // true once anyone has read this file
}

// Watcher polls S3 for file changes and maintains an in-memory file store.
type Watcher struct {
	s3       *simples3.S3
	interval time.Duration
	files    []FileEntry
	logger   *slog.Logger
	loadOnStart bool

	// filesByName maps file name -> FileEntry for quick lookup.
	filesByName map[string]FileEntry

	// mu protects callbacks slice.
	mu        sync.RWMutex
	callbacks []OnUpdateFunc

	// states maps file name -> state.
	states map[string]*fileState
}

// New creates a new Watcher from the given config.
func New(cfg Config) (*Watcher, error) {
	if cfg.S3 == nil {
		return nil, fmt.Errorf("s3watcher: S3 client is required")
	}
	if len(cfg.Files) == 0 {
		return nil, fmt.Errorf("s3watcher: at least one file entry is required")
	}

	interval := cfg.PollInterval
	if interval == 0 {
		interval = time.Minute
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	loadOnStart := true
	if cfg.LoadOnStart != nil {
		loadOnStart = *cfg.LoadOnStart
	}

	// Validate no duplicate names.
	seen := make(map[string]bool, len(cfg.Files))
	for _, f := range cfg.Files {
		if f.Name == "" {
			return nil, fmt.Errorf("s3watcher: file entry name cannot be empty")
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("s3watcher: duplicate file entry name: %s", f.Name)
		}
		seen[f.Name] = true
	}

	states := make(map[string]*fileState, len(cfg.Files))
	filesByName := make(map[string]FileEntry, len(cfg.Files))
	for _, f := range cfg.Files {
		states[f.Name] = &fileState{}
		filesByName[f.Name] = f
	}

	return &Watcher{
		s3:          cfg.S3,
		interval:    interval,
		files:       cfg.Files,
		logger:      logger,
		loadOnStart: loadOnStart,
		filesByName: filesByName,
		states:      states,
	}, nil
}

// OnUpdate registers a callback that fires when any watched file changes.
// Multiple callbacks can be registered; they are called in order.
func (w *Watcher) OnUpdate(fn OnUpdateFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.callbacks = append(w.callbacks, fn)
}

// Get returns the current in-memory contents and ETag for a watched file.
// On first access, it fetches the file from S3 synchronously (lazy load).
// Returns nil, "" if the file is unknown or the fetch fails.
func (w *Watcher) Get(name string) (data []byte, etag string) {
	st, ok := w.states[name]
	if !ok {
		return nil, ""
	}

	// Lazy load: if never accessed, fetch now.
	w.ensureLoaded(name, st)

	st.mu.RLock()
	defer st.mu.RUnlock()

	if st.data == nil {
		return nil, st.etag
	}
	cp := make([]byte, len(st.data))
	copy(cp, st.data)
	return cp, st.etag
}

// Clear evicts a file from the in-memory cache, resetting it to the
// unloaded state. The next Get() or FS read will lazy-load it again.
// The poll loop also stops watching the file until it's re-accessed.
func (w *Watcher) Clear(name string) {
	st, ok := w.states[name]
	if !ok {
		return
	}

	st.mu.Lock()
	st.data = nil
	st.etag = ""
	st.accessed = false
	st.mu.Unlock()

	w.logger.Info("s3watcher: cleared", "name", name)
}

// ClearAll evicts all files from cache.
func (w *Watcher) ClearAll() {
	for _, f := range w.files {
		st := w.states[f.Name]
		st.mu.Lock()
		st.data = nil
		st.etag = ""
		st.accessed = false
		st.mu.Unlock()
	}

	w.logger.Info("s3watcher: cleared all", "count", len(w.files))
}

// Refresh forces an immediate re-fetch of a file from S3, regardless of
// ETag. Useful when you know the file has changed and don't want to wait
// for the next poll. The file is marked as accessed after refresh.
func (w *Watcher) Refresh(name string) error {
	fe, ok := w.filesByName[name]
	if !ok {
		return fmt.Errorf("s3watcher: unknown file: %s", name)
	}

	st := w.states[name]

	// Reset etag so fetchOne always downloads.
	st.mu.Lock()
	st.etag = ""
	st.accessed = true
	st.mu.Unlock()

	w.fetchOne(fe)

	// Check if fetch succeeded.
	st.mu.RLock()
	loaded := st.data != nil
	st.mu.RUnlock()

	if !loaded {
		return fmt.Errorf("s3watcher: refresh failed for %s", name)
	}
	return nil
}

// ensureLoaded fetches the file from S3 if it hasn't been accessed yet.
// Safe to call from multiple goroutines - only the first caller fetches,
// others wait and read the result.
func (w *Watcher) ensureLoaded(name string, st *fileState) {
	st.mu.RLock()
	if st.accessed {
		st.mu.RUnlock()
		return
	}
	st.mu.RUnlock()

	// Upgrade to write lock and double-check.
	st.mu.Lock()
	if st.accessed {
		st.mu.Unlock()
		return
	}
	// Mark accessed under write lock so other goroutines see it
	// and don't also try to fetch.
	st.accessed = true
	st.mu.Unlock()

	// Fetch outside the lock.
	fe := w.filesByName[name]
	w.fetchOne(fe)
}

// Start begins the polling loop. It blocks until ctx is cancelled.
// If LoadOnStart is true (default), it fetches all files before entering
// the poll loop, marking them all as accessed.
func (w *Watcher) Start(ctx context.Context) error {
	if w.loadOnStart {
		w.logger.Info("s3watcher: loading files on start", "count", len(w.files))
		for _, f := range w.files {
			st := w.states[f.Name]
			st.mu.Lock()
			st.accessed = true
			st.mu.Unlock()
		}
		w.pollAll(ctx)
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.logger.Info("s3watcher: polling started", "interval", w.interval, "files", len(w.files))

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("s3watcher: stopped")
			return ctx.Err()
		case <-ticker.C:
			w.pollAll(ctx)
		}
	}
}

// pollAll checks each accessed file for changes. Files that have never
// been read are skipped.
func (w *Watcher) pollAll(ctx context.Context) {
	for _, f := range w.files {
		if ctx.Err() != nil {
			return
		}

		st := w.states[f.Name]
		st.mu.RLock()
		active := st.accessed
		st.mu.RUnlock()

		if !active {
			continue
		}

		w.pollOne(f)
	}
}

// pollOne checks a single file for changes via HEAD, downloads only if
// the ETag has changed.
func (w *Watcher) pollOne(f FileEntry) {
	st := w.states[f.Name]

	// HEAD request to get current ETag.
	details, err := w.s3.FileDetails(simples3.DetailsInput{
		Bucket:    f.Bucket,
		ObjectKey: f.Key,
	})
	if err != nil {
		w.logger.Error("s3watcher: HEAD failed",
			"name", f.Name,
			"bucket", f.Bucket,
			"key", f.Key,
			"error", err,
		)
		return
	}

	// Compare ETag - if unchanged, skip.
	st.mu.RLock()
	currentEtag := st.etag
	st.mu.RUnlock()

	if currentEtag == details.Etag && currentEtag != "" {
		w.logger.Debug("s3watcher: unchanged", "name", f.Name, "etag", details.Etag)
		return
	}

	// ETag changed (or first load) - download the file.
	w.logger.Info("s3watcher: change detected, downloading",
		"name", f.Name,
		"old_etag", currentEtag,
		"new_etag", details.Etag,
	)

	w.downloadAndStore(f, details.Etag, details.LastModified)
}

// fetchOne does an unconditional HEAD + GET for initial lazy load.
func (w *Watcher) fetchOne(f FileEntry) {
	details, err := w.s3.FileDetails(simples3.DetailsInput{
		Bucket:    f.Bucket,
		ObjectKey: f.Key,
	})
	if err != nil {
		w.logger.Error("s3watcher: lazy load HEAD failed",
			"name", f.Name,
			"bucket", f.Bucket,
			"key", f.Key,
			"error", err,
		)
		return
	}

	w.logger.Info("s3watcher: lazy load, downloading",
		"name", f.Name,
		"etag", details.Etag,
	)

	w.downloadAndStore(f, details.Etag, details.LastModified)
}

// downloadAndStore fetches the object body and updates in-memory state + fires callbacks.
func (w *Watcher) downloadAndStore(f FileEntry, etag, lastModified string) {
	body, err := w.s3.FileDownload(simples3.DownloadInput{
		Bucket:    f.Bucket,
		ObjectKey: f.Key,
	})
	if err != nil {
		w.logger.Error("s3watcher: download failed",
			"name", f.Name,
			"error", err,
		)
		return
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		w.logger.Error("s3watcher: read body failed",
			"name", f.Name,
			"error", err,
		)
		return
	}

	// Update in-memory state.
	st := w.states[f.Name]
	st.mu.Lock()
	st.data = data
	st.etag = etag
	st.mu.Unlock()

	w.logger.Info("s3watcher: file updated",
		"name", f.Name,
		"size", len(data),
		"etag", etag,
	)

	// Fire callbacks.
	event := UpdateEvent{
		Name:         f.Name,
		Data:         data,
		ETag:         etag,
		LastModified: lastModified,
	}

	w.mu.RLock()
	cbs := w.callbacks
	w.mu.RUnlock()

	for _, cb := range cbs {
		cb(event)
	}
}
