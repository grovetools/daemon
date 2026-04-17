package env

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/grovetools/core/logging"
	"github.com/sirupsen/logrus"
)

// ProxyManager dynamically routes *.grove.local traffic to allocated ephemeral ports.
type ProxyManager struct {
	mu     sync.RWMutex
	routes map[string]string // e.g., "api.demo.grove.local" -> "127.0.0.1:34012"
	ulog   *logging.UnifiedLogger
}

// NewProxyManager creates a new proxy manager.
// The logger parameter is retained for backwards compatibility and will be
// removed in a later phase.
func NewProxyManager(_ *logrus.Entry) *ProxyManager {
	return &ProxyManager{
		routes: make(map[string]string),
		ulog:   logging.NewUnifiedLogger("groved.env.proxy"),
	}
}

// Register maps a hostname to a local port.
func (pm *ProxyManager) Register(worktree, service string, port int) {
	host := service + "." + worktree + ".grove.local"
	target := fmt.Sprintf("127.0.0.1:%d", port)

	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.routes[host] = target
	pm.ulog.Debug("Route registered").
		Field("host", host).
		Field("target", target).
		Log(context.Background())
}

// Unregister removes all routes matching a specific worktree.
func (pm *ProxyManager) Unregister(worktree string) {
	suffix := "." + worktree + ".grove.local"
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for host := range pm.routes {
		if strings.HasSuffix(host, suffix) {
			delete(pm.routes, host)
		}
	}
}

// Lookup returns the target address for a given host, if registered.
func (pm *ProxyManager) Lookup(host string) (string, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	target, ok := pm.routes[host]
	return target, ok
}

// ListenAndServe starts the proxy server in a blocking manner.
func (pm *ProxyManager) ListenAndServe(addr string) error {
	director := func(req *http.Request) {
		pm.mu.RLock()
		target, ok := pm.routes[req.Host]
		pm.mu.RUnlock()

		if ok {
			req.URL.Scheme = "http"
			req.URL.Host = target
			req.Host = target
		}
	}

	proxy := &httputil.ReverseProxy{Director: director}
	pm.ulog.Info("Proxy server listening").Field("addr", addr).Log(context.Background())
	return http.ListenAndServe(addr, proxy)
}
