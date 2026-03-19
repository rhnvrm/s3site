package s3site

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rhnvrm/simples3"
)

// Handler returns an http.Handler that routes requests by Host header
// to the appropriate site's in-memory filesystem.
//
// The adminHost (if non-empty) serves the admin UI and API on that hostname:
//   - GET  /             - admin UI
//   - GET  /api/sites    - JSON list of hosted sites
//   - POST /api/upload   - upload a tar.gz (multipart: file + hostname)
//   - POST /api/delete   - delete a site (JSON: {"hostname": "..."})
//   - GET  /health       - health check
//
// All other hosts are routed to their matching site's in-memory FS.
func Handler(sm *SiteManager, logger *slog.Logger, adminHost string) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip port from Host header.
		host := r.Host
		if i := strings.LastIndex(host, ":"); i != -1 {
			host = host[:i]
		}

		// Admin host gets the admin UI/API.
		if adminHost != "" && host == adminHost {
			handleAdmin(sm, logger, w, r)
			return
		}

		siteFS := sm.GetSite(host)
		if siteFS == nil {
			logger.Warn("s3site: unknown host", "host", host, "path", r.URL.Path)
			http.Error(w, fmt.Sprintf("site not found: %s", host), http.StatusNotFound)
			return
		}

		http.FileServerFS(siteFS).ServeHTTP(w, r)
	})
}

func handleAdmin(sm *SiteManager, logger *slog.Logger, w http.ResponseWriter, r *http.Request) {
	// CORS for cross-origin access (e.g. landing page fetching /api/sites).
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch r.URL.Path {
	case "/api/sites":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sm.Hostnames())

	case "/api/upload":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.ParseMultipartForm(100 << 20) // 100MB max
		hostname := r.FormValue("hostname")
		if hostname == "" {
			http.Error(w, "hostname is required", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file is required: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()

		key := sm.Prefix() + hostname + ".tar.gz"
		_, err = sm.S3().FilePut(simples3.UploadInput{
			Bucket:      sm.Bucket(),
			ObjectKey:   key,
			ContentType: "application/gzip",
			Body:        file,
		})
		if err != nil {
			logger.Error("s3site admin: upload failed", "key", key, "error", err)
			http.Error(w, "upload failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info("s3site admin: uploaded", "hostname", hostname, "key", key)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "hostname": hostname, "key": key})

	case "/api/delete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Hostname string `json:"hostname"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Hostname == "" {
			http.Error(w, "hostname is required", http.StatusBadRequest)
			return
		}

		key := sm.Prefix() + req.Hostname + ".tar.gz"
		err := sm.S3().FileDelete(simples3.DeleteInput{
			Bucket:    sm.Bucket(),
			ObjectKey: key,
		})
		if err != nil {
			logger.Error("s3site admin: delete failed", "key", key, "error", err)
			http.Error(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info("s3site admin: deleted", "hostname", req.Hostname, "key", key)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "hostname": req.Hostname})

	case "/health":
		fmt.Fprint(w, "ok")

	case "/", "":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, adminHTML)

	default:
		http.NotFound(w, r)
	}
}

const adminHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>s3site admin</title>
  <link rel="stylesheet" href="https://unpkg.com/@knadh/oat@latest/oat.min.css">
</head>
<body>
  <div class="container">
    <h1>s3site admin</h1>
    <p>Static sites served from tar.gz archives in S3.</p>

    <h2>Sites <span id="count"></span></h2>
    <table id="sites-table">
      <thead><tr><th>Hostname</th><th></th></tr></thead>
      <tbody id="sites"></tbody>
    </table>

    <h2>Deploy a site</h2>
    <form onsubmit="return uploadSite(event)">
      <label>Hostname</label>
      <input type="text" id="hostname" placeholder="docs.example.com" required>
      <label>Archive (.tar.gz)</label>
      <input type="file" id="file" accept=".tar.gz,.tgz" required>
      <button type="submit">Upload</button>
    </form>

    <div id="status"></div>
  </div>

  <script>
    async function loadSites() {
      const resp = await fetch('/api/sites');
      const sites = await resp.json();
      document.getElementById('count').textContent = '(' + sites.length + ')';
      const el = document.getElementById('sites');

      if (!sites.length) {
        el.innerHTML = '<tr><td colspan="2">No sites deployed yet.</td></tr>';
        return;
      }

      el.innerHTML = sites.map(h => {
        const url = location.protocol + '//' + h;
        return '<tr>' +
          '<td><a href="' + url + '" target="_blank">' + h + '</a></td>' +
          '<td><button class="small outline" data-variant="danger" onclick="deleteSite(\'' + h + '\')">delete</button></td>' +
          '</tr>';
      }).join('');
    }

    async function deleteSite(hostname) {
      if (!confirm('Delete ' + hostname + '?')) return;
      const resp = await fetch('/api/delete', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({hostname})
      });
      const data = await resp.json();
      showStatus(resp.ok ? 'Deleted ' + hostname + '. Will disappear on next poll.' : 'Error: ' + JSON.stringify(data), resp.ok);
      if (resp.ok) setTimeout(loadSites, 16000);
    }

    async function uploadSite(e) {
      e.preventDefault();
      const hostname = document.getElementById('hostname').value.trim();
      const file = document.getElementById('file').files[0];
      if (!hostname || !file) return;

      const fd = new FormData();
      fd.append('hostname', hostname);
      fd.append('file', file);

      showStatus('Uploading...', true);
      const resp = await fetch('/api/upload', {method: 'POST', body: fd});
      const data = await resp.json();
      if (resp.ok) {
        showStatus('Uploaded! <a href="' + location.protocol + '//' + hostname + '" target="_blank">Open ' + hostname + '</a> (live in ~15s)', true);
        document.getElementById('hostname').value = '';
        document.getElementById('file').value = '';
        setTimeout(loadSites, 16000);
      } else {
        showStatus('Error: ' + JSON.stringify(data), false);
      }
    }

    function showStatus(msg, ok) {
      document.getElementById('status').innerHTML =
        '<p role="alert" data-variant="' + (ok ? 'success' : 'danger') + '">' + msg + '</p>';
    }

    loadSites();
    setInterval(loadSites, 30000);
  </script>
</body>
</html>`
