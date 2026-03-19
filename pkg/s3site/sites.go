package s3site

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rhnvrm/simples3"
)

// StorageMode controls whether extracted sites are kept in memory or on disk.
type StorageMode int

const (
	// StorageMemory keeps all site files in memory. Fast, but uses RAM
	// proportional to the total uncompressed size of all sites.
	StorageMemory StorageMode = iota

	// StorageDisk extracts sites to a directory on disk and serves via
	// os.DirFS. Uses minimal RAM but requires disk space and has disk
	// I/O on every request.
	StorageDisk
)

// site holds the extracted contents of a single tar.gz site archive.
type site struct {
	hostname string
	etag     string
	fs       fs.FS
	files    int    // number of files extracted
	dir      string // disk path (only set in disk mode, for cleanup)
}

// cleanup removes the on-disk directory if one exists.
func (s *site) cleanup() {
	if s.dir != "" {
		os.RemoveAll(s.dir)
	}
}

// SiteManager discovers, downloads, and extracts tar.gz site archives
// from an S3 bucket. Each archive's filename (minus .tar.gz) is treated
// as the hostname it serves.
type SiteManager struct {
	s3       *simples3.S3
	bucket   string
	prefix   string // optional key prefix (e.g. "sites/")
	interval time.Duration
	logger   *slog.Logger
	storage  StorageMode
	dataDir  string // base directory for disk-mode extraction

	mu    sync.RWMutex
	sites map[string]*site // hostname -> site
}

// SiteManagerConfig configures the SiteManager.
type SiteManagerConfig struct {
	S3       *simples3.S3
	Bucket   string
	Prefix   string        // optional S3 key prefix
	Interval time.Duration // poll interval, default 1 minute
	Logger   *slog.Logger

	// Storage controls where extracted sites are kept.
	// Default is StorageMemory.
	Storage StorageMode

	// DataDir is the base directory for disk-mode extraction.
	// Each site gets a subdirectory. Ignored in memory mode.
	// Default is os.TempDir()/s3site-data.
	DataDir string
}

// S3 returns the underlying S3 client.
func (sm *SiteManager) S3() *simples3.S3 { return sm.s3 }

// Bucket returns the configured bucket name.
func (sm *SiteManager) Bucket() string { return sm.bucket }

// Prefix returns the configured key prefix.
func (sm *SiteManager) Prefix() string { return sm.prefix }

// NewSiteManager creates a new SiteManager.
func NewSiteManager(cfg SiteManagerConfig) *SiteManager {
	interval := cfg.Interval
	if interval == 0 {
		interval = time.Minute
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	dataDir := cfg.DataDir
	if dataDir == "" && cfg.Storage == StorageDisk {
		dataDir = filepath.Join(os.TempDir(), "s3site-data")
	}
	return &SiteManager{
		s3:       cfg.S3,
		bucket:   cfg.Bucket,
		prefix:   cfg.Prefix,
		interval: interval,
		logger:   logger,
		storage:  cfg.Storage,
		dataDir:  dataDir,
		sites:    make(map[string]*site),
	}
}

// GetSite returns the fs.FS for a hostname, or nil if not found.
func (sm *SiteManager) GetSite(hostname string) fs.FS {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.sites[hostname]
	if !ok {
		return nil
	}
	return s.fs
}

// Hostnames returns all currently loaded hostnames.
func (sm *SiteManager) Hostnames() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	hosts := make([]string, 0, len(sm.sites))
	for h := range sm.sites {
		hosts = append(hosts, h)
	}
	return hosts
}

// Start runs the poll loop. It does an initial sync, then polls at the
// configured interval. Blocks until ctx is cancelled.
func (sm *SiteManager) Start(ctx context.Context) error {
	mode := "memory"
	if sm.storage == StorageDisk {
		mode = "disk"
		os.MkdirAll(sm.dataDir, 0755)
	}
	sm.logger.Info("s3site: initial sync",
		"bucket", sm.bucket,
		"prefix", sm.prefix,
		"storage", mode,
	)
	sm.sync()

	ticker := time.NewTicker(sm.interval)
	defer ticker.Stop()

	sm.logger.Info("s3site: polling started", "interval", sm.interval)
	for {
		select {
		case <-ctx.Done():
			sm.logger.Info("s3site: stopped")
			return ctx.Err()
		case <-ticker.C:
			sm.sync()
		}
	}
}

// sync lists the bucket for *.tar.gz files, compares ETags, and
// downloads/extracts any new or changed archives.
func (sm *SiteManager) sync() {
	// List all objects with the configured prefix.
	objects, err := sm.listArchives()
	if err != nil {
		sm.logger.Error("s3site: list failed", "error", err)
		return
	}

	// Track which hostnames are still present.
	seen := make(map[string]bool, len(objects))

	for _, obj := range objects {
		hostname := archiveToHostname(obj.Key, sm.prefix)
		if hostname == "" {
			continue
		}
		seen[hostname] = true

		// Check if we already have this version.
		sm.mu.RLock()
		existing, loaded := sm.sites[hostname]
		sm.mu.RUnlock()

		if loaded && existing.etag == obj.ETag {
			sm.logger.Debug("s3site: unchanged", "host", hostname)
			continue
		}

		// Download and extract.
		sm.logger.Info("s3site: downloading",
			"host", hostname,
			"key", obj.Key,
			"old_etag", existingEtag(existing),
			"new_etag", obj.ETag,
		)

		newSite, err := sm.downloadAndExtract(hostname, obj.Key)
		if err != nil {
			sm.logger.Error("s3site: extract failed",
				"host", hostname,
				"key", obj.Key,
				"error", err,
			)
			continue
		}
		newSite.etag = obj.ETag

		sm.mu.Lock()
		old := sm.sites[hostname]
		sm.sites[hostname] = newSite
		sm.mu.Unlock()

		// Clean up old disk directory after swap.
		if old != nil {
			old.cleanup()
		}

		sm.logger.Info("s3site: site loaded",
			"host", hostname,
			"files", newSite.files,
			"etag", obj.ETag,
		)
	}

	// Remove sites whose archives were deleted from S3.
	sm.mu.Lock()
	for hostname, s := range sm.sites {
		if !seen[hostname] {
			sm.logger.Info("s3site: site removed", "host", hostname)
			s.cleanup()
			delete(sm.sites, hostname)
		}
	}
	sm.mu.Unlock()
}

// archiveObject holds the key and etag from a bucket listing.
type archiveObject struct {
	Key  string
	ETag string
}

// listArchives returns all *.tar.gz objects in the bucket.
func (sm *SiteManager) listArchives() ([]archiveObject, error) {
	var archives []archiveObject

	var contToken string
	for {
		resp, err := sm.s3.List(simples3.ListInput{
			Bucket:            sm.bucket,
			Prefix:            sm.prefix,
			ContinuationToken: contToken,
			MaxKeys:           1000,
		})
		if err != nil {
			return nil, err
		}

		for _, obj := range resp.Objects {
			if strings.HasSuffix(obj.Key, ".tar.gz") {
				archives = append(archives, archiveObject{
					Key:  obj.Key,
					ETag: obj.ETag,
				})
			}
		}

		if !resp.IsTruncated {
			break
		}
		contToken = resp.NextContinuationToken
	}

	return archives, nil
}

// downloadAndExtract fetches a tar.gz from S3 and extracts it.
// In memory mode, files go into a memFS. In disk mode, files go to dataDir/{hostname}/.
func (sm *SiteManager) downloadAndExtract(hostname, key string) (*site, error) {
	body, err := sm.s3.FileDownload(simples3.DownloadInput{
		Bucket:    sm.bucket,
		ObjectKey: key,
	})
	if err != nil {
		return nil, err
	}
	defer body.Close()

	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	if sm.storage == StorageDisk {
		return sm.extractToDisk(hostname, tr)
	}
	return sm.extractToMemory(hostname, tr)
}

func (sm *SiteManager) extractToMemory(hostname string, tr *tar.Reader) (*site, error) {
	mfs := newMemFS()

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := cleanTarPath(hdr.Name)
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		mfs.Add(name, data)
	}

	return &site{
		hostname: hostname,
		fs:       mfs,
		files:    mfs.Len(),
	}, nil
}

func (sm *SiteManager) extractToDisk(hostname string, tr *tar.Reader) (*site, error) {
	// Extract to a temp dir first, then rename. This avoids serving
	// a half-extracted site if the download fails partway.
	tmpDir, err := os.MkdirTemp(sm.dataDir, hostname+"-tmp-*")
	if err != nil {
		return nil, err
	}

	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.RemoveAll(tmpDir)
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		name := cleanTarPath(hdr.Name)
		dest := filepath.Join(tmpDir, filepath.FromSlash(name))

		// Prevent path traversal.
		if !strings.HasPrefix(dest, tmpDir+string(filepath.Separator)) && dest != tmpDir {
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			os.RemoveAll(tmpDir)
			return nil, err
		}

		f, err := os.Create(dest)
		if err != nil {
			os.RemoveAll(tmpDir)
			return nil, err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			os.RemoveAll(tmpDir)
			return nil, err
		}
		count++
	}

	// Rename to final location.
	finalDir := filepath.Join(sm.dataDir, hostname)
	os.RemoveAll(finalDir) // remove old version if any
	if err := os.Rename(tmpDir, finalDir); err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}

	return &site{
		hostname: hostname,
		fs:       os.DirFS(finalDir),
		files:    count,
		dir:      finalDir,
	}, nil
}

// cleanTarPath normalizes a tar entry name.
func cleanTarPath(name string) string {
	name = path.Clean(name)
	name = strings.TrimPrefix(name, "/")
	name = strings.TrimPrefix(name, "./")
	return name
}

// archiveToHostname extracts the hostname from a key like
// "prefix/foo.example.com.tar.gz" -> "foo.example.com".
func archiveToHostname(key, prefix string) string {
	name := strings.TrimPrefix(key, prefix)
	if !strings.HasSuffix(name, ".tar.gz") {
		return ""
	}
	return strings.TrimSuffix(name, ".tar.gz")
}

func existingEtag(s *site) string {
	if s == nil {
		return ""
	}
	return s.etag
}
