package s3site

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
)

// HostedSite declares one hosted site and its source object key.
type HostedSite struct {
	Hostname string `json:"hostname"`
	Key      string `json:"key,omitempty"`
}

type hostedSitesFile struct {
	Sites []HostedSite `json:"sites"`
}

// LoadHostedSites reads and validates a hosted-site registry from JSON.
// When a site omits Key, the key defaults to prefix + hostname + ".tar.gz".
func LoadHostedSites(path, prefix string) ([]HostedSite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg hostedSitesFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse hosted sites config: %w", err)
	}
	if len(cfg.Sites) == 0 {
		return nil, fmt.Errorf("hosted sites config must declare at least one site")
	}

	seen := make(map[string]bool, len(cfg.Sites))
	normalized := make([]HostedSite, 0, len(cfg.Sites))
	for _, site := range cfg.Sites {
		hostname, err := canonicalHostname(site.Hostname)
		if err != nil {
			return nil, err
		}
		if seen[hostname] {
			return nil, fmt.Errorf("duplicate hosted site: %s", hostname)
		}
		seen[hostname] = true

		key := strings.TrimSpace(site.Key)
		if key == "" {
			key = prefix + hostname + ".tar.gz"
		}
		normalized = append(normalized, HostedSite{
			Hostname: hostname,
			Key:      key,
		})
	}

	return normalized, nil
}

func canonicalHostname(host string) (string, error) {
	host = normalizeHost(host)
	if err := validateHostname(host); err != nil {
		return "", fmt.Errorf("invalid hostname %q: %w", host, err)
	}
	return host, nil
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}

	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	} else if strings.Count(host, ":") == 1 && !strings.HasPrefix(host, "[") {
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
	}

	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimSuffix(host, ".")
	host = strings.ToLower(host)
	return host
}

func validateHostname(host string) error {
	if host == "" {
		return fmt.Errorf("hostname is required")
	}
	if len(host) > 253 {
		return fmt.Errorf("hostname too long")
	}

	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("hostname has empty label")
		}
		if len(label) > 63 {
			return fmt.Errorf("hostname label too long")
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("hostname labels cannot start or end with '-'")
		}
		for _, ch := range label {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				continue
			}
			return fmt.Errorf("hostname contains invalid character %q", ch)
		}
	}

	return nil
}
