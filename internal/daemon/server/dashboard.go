package server

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/daemon/internal/daemon/dashboard"
)

//go:embed dashboard_assets
var dashboardAssets embed.FS

// dashboardAssetsFS strips the top-level "dashboard_assets" directory so
// handler paths map cleanly: "/dashboard/assets/dashboard.js" -> "dashboard.js".
func dashboardAssetsFS() http.FileSystem {
	sub, err := fs.Sub(dashboardAssets, "dashboard_assets")
	if err != nil {
		// go:embed validated at build time — this can't happen unless the
		// directory structure changes. Panic so the daemon fails fast.
		panic("dashboard_assets embed broken: " + err.Error())
	}
	return http.FS(sub)
}

// registerDashboardRoutes wires the dashboard routes onto mux. Called only
// by the TCP mux built on the dashboard listener, not the unix-socket mux —
// scoped daemons never serve dashboard traffic.
func (s *Server) registerDashboardRoutes(mux *http.ServeMux) {
	agg := dashboard.New(s.envManager)

	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, dashboardAssetsEmbed(), "dashboard_assets/index.html")
	})
	mux.Handle("/dashboard/assets/",
		http.StripPrefix("/dashboard/assets/", http.FileServer(dashboardAssetsFS())))

	mux.HandleFunc("/api/dashboard/state", func(w http.ResponseWriter, r *http.Request) {
		cfg, err := config.LoadDefault()
		if err != nil {
			http.Error(w, "config load: "+err.Error(), http.StatusInternalServerError)
			return
		}
		probe := r.URL.Query().Get("probe") != "0"
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		state := agg.Build(ctx, cfg, probe)
		dashboard.WriteJSON(w, state)
	})
}

// dashboardAssetsEmbed returns the raw embed.FS so ServeFileFS can walk the
// correct relative path ("dashboard_assets/index.html"). Kept separate from
// dashboardAssetsFS() which strips the prefix for the asset directory
// handler.
func dashboardAssetsEmbed() fs.FS { return dashboardAssets }

// DashboardPortFile returns the well-known path clients read to discover the
// active dashboard port. Exported so grove/cmd/env_dashboard can reuse the
// exact same location without duplicating the constant.
func DashboardPortFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "grove", "dashboard.port")
}

// StartDashboard allocates an ephemeral TCP port on 127.0.0.1, serves the
// dashboard routes there, and writes the allocated port to the well-known
// state file. Returns the resolved TCP address (e.g. "127.0.0.1:54321").
//
// Gate the caller on scope == "" — scoped daemons should never invoke this.
func (s *Server) StartDashboard(ctx context.Context) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("dashboard listen: %w", err)
	}
	addr := listener.Addr().(*net.TCPAddr)

	mux := http.NewServeMux()
	s.registerDashboardRoutes(mux)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Persist the port so `grove env dashboard` can find us without a
	// broadcast/discovery protocol.
	portPath := DashboardPortFile()
	if portPath != "" {
		if err := os.MkdirAll(filepath.Dir(portPath), 0755); err == nil {
			_ = os.WriteFile(portPath, []byte(fmt.Sprintf("%d\n", addr.Port)), 0644)
		}
	}

	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.ulog.Warn("dashboard server stopped").Err(err).Log(ctx)
		}
	}()

	// Clean up the port file on shutdown — ctx.Done fires from the main
	// signal handler path.
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		if portPath != "" {
			_ = os.Remove(portPath)
		}
	}()

	return addr.String(), nil
}
