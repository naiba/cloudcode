package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// ReverseProxy manages dynamic reverse proxying to opencode instances.
type ReverseProxy struct {
	mu      sync.RWMutex
	proxies map[string]*httputil.ReverseProxy // instanceID → proxy
	ports   map[string]int                    // instanceID → port
}

// New creates a new ReverseProxy manager.
func New() *ReverseProxy {
	return &ReverseProxy{
		proxies: make(map[string]*httputil.ReverseProxy),
		ports:   make(map[string]int),
	}
}

// Register adds or updates a proxy route for an instance.
func (rp *ReverseProxy) Register(instanceID string, port int) error {
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("parse target URL: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Custom director to strip the instance prefix path
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Strip /instance/{id}/ prefix before forwarding
		prefix := fmt.Sprintf("/instance/%s", instanceID)
		if strings.HasPrefix(req.URL.Path, prefix) {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
		}
		req.Host = target.Host
	}

	// Handle WebSocket upgrade
	proxy.ModifyResponse = func(resp *http.Response) error {
		return nil
	}

	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.proxies[instanceID] = proxy
	rp.ports[instanceID] = port

	return nil
}

// Unregister removes a proxy route.
func (rp *ReverseProxy) Unregister(instanceID string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	delete(rp.proxies, instanceID)
	delete(rp.ports, instanceID)
}

// ServeHTTP handles proxied requests.
func (rp *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request, instanceID string) {
	rp.mu.RLock()
	proxy, ok := rp.proxies[instanceID]
	rp.mu.RUnlock()

	if !ok {
		http.Error(w, "Instance not found or not running", http.StatusBadGateway)
		return
	}

	proxy.ServeHTTP(w, r)
}

// IsRegistered checks if an instance has a registered proxy.
func (rp *ReverseProxy) IsRegistered(instanceID string) bool {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	_, ok := rp.proxies[instanceID]
	return ok
}
