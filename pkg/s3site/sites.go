package s3site

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rhnvrm/simples3"
)

const (
	defaultMaxArchiveFiles = 10_000
	defaultMaxArchiveBytes = int64(1 << 30) // 1 GiB
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
	hostname      string
	key           string
	etag          string
	fs            fs.FS
	files         int
	dir           string
	retainedDirs  []string
	lastActivated time.Time
}

// cleanup removes the on-disk directory and any retained older versions.
func (s *site) cleanup() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
	for _, dir := range s.retainedDirs {
		_ = os.RemoveAll(dir)
	}
}

// SiteManager discovers, downloads, and extracts tar.gz site archives
// from an S3 bucket. Each archive's filename (minus .tar.gz) is treated
// as the hostname it serves.
type SiteManager struct {
	s3              *simples3.S3
	bucket          string
	prefix          string // optional key prefix (e.g. "sites/")
	interval        time.Duration
	logger          *slog.Logger
	storage         StorageMode
	dataDir         string // base directory for disk-mode extraction
	declaredSites   map[string]HostedSite
	maxArchiveFiles int
	maxArchiveBytes int64
	configErr       error

	syncMu sync.Mutex
	mu     sync.RWMutex
	sites  map[string]*site // hostname -> site

	stateMu         sync.RWMutex
	initialSyncDone bool
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

	// HostedSites declares the only sites that may be served in hosted mode.
	// When empty, s3site uses legacy auto-discovery by listing *.tar.gz files.
	HostedSites []HostedSite

	// MaxArchiveFiles limits the number of regular files extracted per site.
	// Default is 10,000.
	MaxArchiveFiles int

	// MaxArchiveBytes limits the total uncompressed size extracted per site.
	// Default is 1 GiB.
	MaxArchiveBytes int64
}

// S3 returns the underlying S3 client.
func (sm *SiteManager) S3() *simples3.S3 { return sm.s3 }

// Bucket returns the configured bucket name.
func (sm *SiteManager) Bucket() string { return sm.bucket }

// Prefix returns the configured key prefix.
func (sm *SiteManager) Prefix() string { return sm.prefix }

// HostedMode reports whether the manager is using an explicit site registry.
func (sm *SiteManager) HostedMode() bool { return len(sm.declaredSites) > 0 }

// Ready reports whether the initial sync attempt has completed.
func (sm *SiteManager) Ready() bool {
	sm.stateMu.RLock()
	defer sm.stateMu.RUnlock()
	return sm.initialSyncDone
}

// Validate reports any configuration error detected during construction.
func (sm *SiteManager) Validate() error { return sm.configErr }

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
	maxArchiveFiles := cfg.MaxArchiveFiles
	if maxArchiveFiles == 0 {
		maxArchiveFiles = defaultMaxArchiveFiles
	}
	maxArchiveBytes := cfg.MaxArchiveBytes
	if maxArchiveBytes == 0 {
		maxArchiveBytes = defaultMaxArchiveBytes
	}

	declaredSites := make(map[string]HostedSite, len(cfg.HostedSites))
	var configErr error
	for _, site := range cfg.HostedSites {
		hostname, err := canonicalHostname(site.Hostname)
		if err != nil {
			configErr = errors.Join(configErr, err)
			continue
		}
		key := strings.TrimSpace(site.Key)
		if key == "" {
			key = cfg.Prefix + hostname + ".tar.gz"
		}
		if _, exists := declaredSites[hostname]; exists {
			configErr = errors.Join(configErr, fmt.Errorf("s3site: duplicate hosted site: %s", hostname))
			continue
		}
		declaredSites[hostname] = HostedSite{Hostname: hostname, Key: key}
	}
	if cfg.S3 == nil {
		configErr = errors.Join(configErr, fmt.Errorf("s3site: S3 client is required"))
	}
	if cfg.Bucket == "" {
		configErr = errors.Join(configErr, fmt.Errorf("s3site: bucket is required"))
	}
	if interval < 0 {
		configErr = errors.Join(configErr, fmt.Errorf("s3site: poll interval must be >= 0"))
	}
	if maxArchiveFiles <= 0 {
		configErr = errors.Join(configErr, fmt.Errorf("s3site: max archive files must be > 0"))
	}
	if maxArchiveBytes <= 0 {
		configErr = errors.Join(configErr, fmt.Errorf("s3site: max archive bytes must be > 0"))
	}

	return &SiteManager{
		s3:              cfg.S3,
		bucket:          cfg.Bucket,
		prefix:          cfg.Prefix,
		interval:        interval,
		logger:          logger,
		storage:         cfg.Storage,
		dataDir:         dataDir,
		declaredSites:   declaredSites,
		maxArchiveFiles: maxArchiveFiles,
		maxArchiveBytes: maxArchiveBytes,
		configErr:       configErr,
		sites:           make(map[string]*site),
	}
}

// GetSite returns the fs.FS for a hostname, or nil if not found.
func (sm *SiteManager) GetSite(hostname string) fs.FS {
	hostname = normalizeHost(hostname)
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
	sort.Strings(hosts)
	return hosts
}

// RefreshHosts forces refresh for the provided declared hosts.
func (sm *SiteManager) RefreshHosts(hostnames []string) error {
	if !sm.HostedMode() {
		return fmt.Errorf("s3site: refresh is only supported in hosted mode")
	}
	if len(hostnames) == 0 {
		return fmt.Errorf("s3site: at least one hostname is required")
	}

	forceHosts := make(map[string]bool, len(hostnames))
	for _, host := range hostnames {
		normalized, err := canonicalHostname(host)
		if err != nil {
			return err
		}
		if _, ok := sm.declaredSites[normalized]; !ok {
			return fmt.Errorf("s3site: hostname is not declared: %s", normalized)
		}
		forceHosts[normalized] = true
	}

	sm.syncMu.Lock()
	defer sm.syncMu.Unlock()
	return sm.syncDeclared(forceHosts, true)
}

// Start runs the poll loop. It does an initial sync, then polls at the
// configured interval. Blocks until ctx is cancelled.
func (sm *SiteManager) Start(ctx context.Context) error {
	if sm.configErr != nil {
		return sm.configErr
	}

	mode := "memory"
	if sm.storage == StorageDisk {
		mode = "disk"
		if err := os.MkdirAll(sm.dataDir, 0755); err != nil {
			return err
		}
	}

	startupMode := "discovery"
	if sm.HostedMode() {
		startupMode = "hosted"
	}
	sm.logger.Info("s3site: initial sync",
		"bucket", sm.bucket,
		"prefix", sm.prefix,
		"storage", mode,
		"mode", startupMode,
	)

	sm.syncMu.Lock()
	initErr := sm.syncOnce()
	loadedSites := sm.loadedSiteCount()
	sm.syncMu.Unlock()
	if initErr != nil {
		if loadedSites == 0 {
			return fmt.Errorf("s3site: initial sync failed with no loaded sites: %w", initErr)
		}
		sm.logger.Error("s3site: initial sync incomplete; continuing with last-good/partial state", "error", initErr, "loaded_sites", loadedSites)
	}

	sm.stateMu.Lock()
	sm.initialSyncDone = true
	sm.stateMu.Unlock()

	if sm.interval == 0 {
		<-ctx.Done()
		sm.logger.Info("s3site: stopped")
		return ctx.Err()
	}

	ticker := time.NewTicker(sm.interval)
	defer ticker.Stop()

	sm.logger.Info("s3site: polling started", "interval", sm.interval)
	for {
		select {
		case <-ctx.Done():
			sm.logger.Info("s3site: stopped")
			return ctx.Err()
		case <-ticker.C:
			sm.syncMu.Lock()
			if err := sm.syncOnce(); err != nil {
				sm.logger.Error("s3site: poll sync failed", "error", err)
			}
			sm.syncMu.Unlock()
		}
	}
}

func (sm *SiteManager) syncOnce() error {
	if sm.HostedMode() {
		return sm.syncDeclared(nil, false)
	}
	return sm.syncDiscovered()
}

func (sm *SiteManager) syncDeclared(forceHosts map[string]bool, force bool) error {
	hostnames := make([]string, 0, len(sm.declaredSites))
	for hostname := range sm.declaredSites {
		hostnames = append(hostnames, hostname)
	}
	sort.Strings(hostnames)

	var errs []error
	for _, hostname := range hostnames {
		if forceHosts != nil && !forceHosts[hostname] {
			continue
		}
		if err := sm.syncDeclaredSite(sm.declaredSites[hostname], force); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (sm *SiteManager) syncDeclaredSite(spec HostedSite, force bool) error {
	details, err := sm.s3.FileDetails(simples3.DetailsInput{
		Bucket:    sm.bucket,
		ObjectKey: spec.Key,
	})
	if err != nil {
		sm.logger.Error("s3site: site details failed",
			"host", spec.Hostname,
			"key", spec.Key,
			"error", err,
		)
		return fmt.Errorf("site %s: details failed: %w", spec.Hostname, err)
	}

	sm.mu.RLock()
	existing, loaded := sm.sites[spec.Hostname]
	sm.mu.RUnlock()

	if !force && loaded && existing.etag == details.Etag && details.Etag != "" {
		sm.logger.Debug("s3site: unchanged", "host", spec.Hostname)
		return nil
	}

	sm.logger.Info("s3site: downloading",
		"host", spec.Hostname,
		"key", spec.Key,
		"old_etag", existingEtag(existing),
		"new_etag", details.Etag,
		"forced", force,
	)

	newSite, err := sm.downloadAndExtract(spec.Hostname, spec.Key)
	if err != nil {
		sm.logger.Error("s3site: extract failed",
			"host", spec.Hostname,
			"key", spec.Key,
			"error", err,
		)
		return fmt.Errorf("site %s: extract failed: %w", spec.Hostname, err)
	}
	newSite.key = spec.Key
	newSite.etag = details.Etag
	newSite.lastActivated = time.Now()

	cleanupDirs := sm.activateSite(spec.Hostname, newSite)
	cleanupOldDirs(cleanupDirs)

	sm.logger.Info("s3site: site loaded",
		"host", spec.Hostname,
		"files", newSite.files,
		"etag", details.Etag,
	)
	return nil
}

// syncDiscovered lists the bucket for *.tar.gz files, compares ETags, and
// downloads/extracts any new or changed archives.
func (sm *SiteManager) syncDiscovered() error {
	objects, err := sm.listArchives()
	if err != nil {
		return err
	}

	seen := make(map[string]bool, len(objects))

	for _, obj := range objects {
		hostname := archiveToHostname(obj.Key, sm.prefix)
		if hostname == "" {
			continue
		}
		seen[hostname] = true

		sm.mu.RLock()
		existing, loaded := sm.sites[hostname]
		sm.mu.RUnlock()

		if loaded && existing.etag == obj.ETag {
			sm.logger.Debug("s3site: unchanged", "host", hostname)
			continue
		}

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
		newSite.key = obj.Key
		newSite.etag = obj.ETag
		newSite.lastActivated = time.Now()

		cleanupDirs := sm.activateSite(hostname, newSite)
		cleanupOldDirs(cleanupDirs)

		sm.logger.Info("s3site: site loaded",
			"host", hostname,
			"files", newSite.files,
			"etag", obj.ETag,
		)
	}

	sm.mu.Lock()
	for hostname, s := range sm.sites {
		if !seen[hostname] {
			sm.logger.Info("s3site: site removed", "host", hostname)
			s.cleanup()
			delete(sm.sites, hostname)
		}
	}
	sm.mu.Unlock()

	return nil
}

func (sm *SiteManager) activateSite(hostname string, newSite *site) []string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	var cleanupDirs []string
	if old := sm.sites[hostname]; old != nil {
		if old.dir != "" {
			newSite.retainedDirs = append(newSite.retainedDirs, old.retainedDirs...)
			newSite.retainedDirs = append(newSite.retainedDirs, old.dir)
			if len(newSite.retainedDirs) > 1 {
				cleanupDirs = append(cleanupDirs, newSite.retainedDirs[:len(newSite.retainedDirs)-1]...)
				newSite.retainedDirs = newSite.retainedDirs[len(newSite.retainedDirs)-1:]
			}
		} else {
			cleanupDirs = append(cleanupDirs, old.retainedDirs...)
		}
	}

	sm.sites[hostname] = newSite
	return cleanupDirs
}

func cleanupOldDirs(dirs []string) {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

func (sm *SiteManager) loadedSiteCount() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sites)
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
// In memory mode, files go into a memFS. In disk mode, files go to a
// versioned directory below dataDir.
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
	count := 0
	var totalBytes int64

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
		if hdr.Size < 0 {
			return nil, fmt.Errorf("invalid negative size for %q", hdr.Name)
		}

		name, err := cleanTarPath(hdr.Name)
		if err != nil {
			return nil, err
		}

		count++
		if count > sm.maxArchiveFiles {
			return nil, fmt.Errorf("archive exceeds file limit: %d", sm.maxArchiveFiles)
		}
		totalBytes += hdr.Size
		if totalBytes > sm.maxArchiveBytes {
			return nil, fmt.Errorf("archive exceeds size limit: %d", sm.maxArchiveBytes)
		}

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
	hostDir := filepath.Join(sm.dataDir, hostname)
	if err := os.MkdirAll(hostDir, 0755); err != nil {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp(hostDir, "site-*")
	if err != nil {
		return nil, err
	}

	count := 0
	var totalBytes int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size < 0 {
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("invalid negative size for %q", hdr.Name)
		}

		name, err := cleanTarPath(hdr.Name)
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, err
		}

		count++
		if count > sm.maxArchiveFiles {
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("archive exceeds file limit: %d", sm.maxArchiveFiles)
		}
		totalBytes += hdr.Size
		if totalBytes > sm.maxArchiveBytes {
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("archive exceeds size limit: %d", sm.maxArchiveBytes)
		}

		dest := filepath.Join(tmpDir, filepath.FromSlash(name))
		if !strings.HasPrefix(dest, tmpDir+string(filepath.Separator)) {
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("invalid archive path: %q", hdr.Name)
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, err
		}

		f, err := os.Create(dest)
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, err
		}
		_, err = io.Copy(f, tr)
		closeErr := f.Close()
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, err
		}
		if closeErr != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, closeErr
		}
	}

	return &site{
		hostname: hostname,
		fs:       os.DirFS(tmpDir),
		files:    count,
		dir:      tmpDir,
	}, nil
}

// cleanTarPath normalizes and validates a tar entry name.
func cleanTarPath(name string) (string, error) {
	cleaned := path.Clean(name)
	cleaned = strings.TrimPrefix(cleaned, "/")
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "." || cleaned == "" || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid archive path: %q", name)
	}
	return cleaned, nil
}

// archiveToHostname extracts the hostname from a key like
// "prefix/foo.example.com.tar.gz" -> "foo.example.com".
func archiveToHostname(key, prefix string) string {
	name := strings.TrimPrefix(key, prefix)
	if !strings.HasSuffix(name, ".tar.gz") {
		return ""
	}
	name = strings.TrimSuffix(name, ".tar.gz")
	hostname, err := canonicalHostname(name)
	if err != nil {
		return ""
	}
	return hostname
}

func existingEtag(s *site) string {
	if s == nil {
		return ""
	}
	return s.etag
}
