package s3site

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type refreshRequest struct {
	Hosts []string `json:"hosts"`
}

type refreshResponse struct {
	Status string   `json:"status"`
	Hosts  []string `json:"hosts"`
}

type healthResponse struct {
	Ready bool `json:"ready"`
}

// StartControlServer serves local-only control operations over a unix socket.
func StartControlServer(ctx context.Context, socketPath string, sm *SiteManager, logger *slog.Logger) error {
	if socketPath == "" {
		return nil
	}
	ln, err := ListenControlSocket(socketPath)
	if err != nil {
		return err
	}
	return ServeControlServer(ctx, socketPath, ln, sm, logger)
}

// ListenControlSocket prepares and binds the unix socket used by the local control plane.
func ListenControlSocket(socketPath string) (net.Listener, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("control socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return nil, err
	}
	if err := removeExistingSocket(socketPath); err != nil {
		return nil, err
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// ServeControlServer serves the local control plane on an already-bound listener.
func ServeControlServer(ctx context.Context, socketPath string, ln net.Listener, sm *SiteManager, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	srv := &http.Server{
		Handler:      controlHandler(sm, logger),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = removeExistingSocket(socketPath)
	}()

	logger.Info("s3site: control socket listening", "socket", socketPath)
	err := srv.Serve(ln)
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func removeExistingSocket(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path: %s", socketPath)
	}
	return os.Remove(socketPath)
}

func controlHandler(sm *SiteManager, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(healthResponse{Ready: sm.Ready()})

		case "/refresh":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req refreshRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				return
			}
			if err := sm.RefreshHosts(req.Hosts); err != nil {
				logger.Error("s3site: refresh failed", "hosts", req.Hosts, "error", err)
				http.Error(w, fmt.Sprintf("refresh failed: %v", err), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(refreshResponse{Status: "ok", Hosts: req.Hosts})

		default:
			http.NotFound(w, r)
		}
	})
}
