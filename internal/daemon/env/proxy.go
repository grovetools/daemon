package env

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// ProxyManager dynamically routes *.grove.local traffic to allocated ephemeral ports.
type ProxyManager struct {
	mu     sync.RWMutex
	routes map[string]string // e.g., "api.demo.grove.local" -> "127.0.0.1:34012"
	logger *logrus.Entry
}

func NewProxyManager(logger *logrus.Entry) *ProxyManager {
	return &ProxyManager{
		routes: make(map[string]string),
		logger: logger.WithField("component", "proxy"),
	}
}

// Register maps a hostname to a local port.
func (pm *ProxyManager) Register(worktree, service string, port int) {
	host := service + "." + worktree + ".grove.local"
	target := fmt.Sprintf("127.0.0.1:%d", port)

	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.routes[host] = target
	pm.logger.WithFields(logrus.Fields{"host": host, "target": target}).Debug("Route registered")
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
	pm.logger.WithField("addr", addr).Info("Proxy server listening")
	return http.ListenAndServe(addr, proxy)
}
