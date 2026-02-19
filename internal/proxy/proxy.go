package proxy

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// ReverseProxy manages dynamic reverse proxying to opencode instances.
type ReverseProxy struct {
	mu      sync.RWMutex
	proxies map[string]*httputil.ReverseProxy // instanceID → proxy (strips /instance/{id} prefix)
	direct  map[string]*httputil.ReverseProxy // instanceID → proxy (forwards path as-is)
	ports   map[string]int                    // instanceID → port
}

// New creates a new ReverseProxy manager.
func New() *ReverseProxy {
	return &ReverseProxy{
		proxies: make(map[string]*httputil.ReverseProxy),
		direct:  make(map[string]*httputil.ReverseProxy),
		ports:   make(map[string]int),
	}
}

// Register adds or updates a proxy route for an instance.
// Traffic is routed via Docker network using container name (cloudcode-{id}).
func (rp *ReverseProxy) Register(instanceID string, port int) error {
	containerName := fmt.Sprintf("cloudcode-%s", instanceID)
	target, err := url.Parse(fmt.Sprintf("http://%s:%d", containerName, port))
	if err != nil {
		return fmt.Errorf("parse target URL: %w", err)
	}

	stripProxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := stripProxy.Director
	stripProxy.Director = func(req *http.Request) {
		originalDirector(req)
		prefix := fmt.Sprintf("/instance/%s", instanceID)
		if strings.HasPrefix(req.URL.Path, prefix) {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
		}
		req.Host = target.Host
		req.Header.Del("Accept-Encoding")
	}
	stripProxy.ModifyResponse = injectInstanceIsolation(instanceID)
	stripProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		tmpl := template.Must(template.New("waiting").Parse(waitingPageHTML))
		_ = tmpl.Execute(w, map[string]string{"InstanceID": instanceID})
	}

	// Proxy that forwards path as-is (for Referer-based fallback requests)
	directProxy := httputil.NewSingleHostReverseProxy(target)
	origDirectDirector := directProxy.Director
	directProxy.Director = func(req *http.Request) {
		origDirectDirector(req)
		req.Host = target.Host
	}
	directProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.proxies[instanceID] = stripProxy
	rp.direct[instanceID] = directProxy
	rp.ports[instanceID] = port

	return nil
}

// Unregister removes a proxy route.
func (rp *ReverseProxy) Unregister(instanceID string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	delete(rp.proxies, instanceID)
	delete(rp.direct, instanceID)
	delete(rp.ports, instanceID)
}

// ServeHTTP handles proxied requests, stripping /instance/{id} prefix.
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

// ServeHTTPDirect handles proxied requests, forwarding the original path as-is.
// Used for Referer-based fallback routing where the path is already correct
// (e.g. /assets/index-xxx.js, /global/health, WebSocket upgrades).
func (rp *ReverseProxy) ServeHTTPDirect(w http.ResponseWriter, r *http.Request, instanceID string) {
	rp.mu.RLock()
	proxy, ok := rp.direct[instanceID]
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

func injectInstanceIsolation(instanceID string) func(*http.Response) error {
	script := `<script>
(function() {
  var K = "_cc_active_inst";
  var ID = "` + instanceID + `";
  var SK = "_cc_store_" + ID;

  function isShared(n) {
    return n === K || n.startsWith("_cc_store_") ||
      n === "theme" || n === "opencode-theme-id" || n === "opencode-color-scheme" ||
      n.startsWith("opencode-theme-css-");
  }

  var toRemove = [];
  for (var i = localStorage.length; i--;) {
    var n = localStorage.key(i);
    if (!isShared(n)) toRemove.push(n);
  }
  toRemove.forEach(function(n) { localStorage.removeItem(n); });

  var saved = localStorage.getItem(SK);
  if (saved) {
    try {
      var d = JSON.parse(saved);
      Object.keys(d).forEach(function(n) { localStorage.setItem(n, d[n]); });
    } catch(e) {}
  }
  localStorage.setItem(K, ID);

  var _set = Storage.prototype.setItem;
  var _rm = Storage.prototype.removeItem;
  var _cl = Storage.prototype.clear;
  var syncing = false;

  function sync() {
    if (syncing) return;
    syncing = true;
    var s = {};
    for (var i = localStorage.length; i--;) {
      var n = localStorage.key(i);
      if (!isShared(n)) s[n] = localStorage.getItem(n);
    }
    _set.call(localStorage, SK, JSON.stringify(s));
    syncing = false;
  }

  Storage.prototype.setItem = function(n, v) {
    _set.call(this, n, v);
    if (this === localStorage && !isShared(n)) sync();
  };
  Storage.prototype.removeItem = function(n) {
    _rm.call(this, n);
    if (this === localStorage && !isShared(n)) sync();
  };
  Storage.prototype.clear = function() {
    _cl.call(this);
    if (this === localStorage) sync();
  };
})();
</script>`

	return func(resp *http.Response) error {
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			return nil
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}

		injection := []byte(script)
		headTag := []byte("<head>")
		idx := bytes.Index(bytes.ToLower(body), headTag)
		if idx == -1 {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return nil
		}

		insertAt := idx + len(headTag)
		modified := make([]byte, 0, len(body)+len(injection))
		modified = append(modified, body[:insertAt]...)
		modified = append(modified, injection...)
		modified = append(modified, body[insertAt:]...)

		resp.Body = io.NopCloser(bytes.NewReader(modified))
		resp.ContentLength = int64(len(modified))
		resp.Header.Set("Content-Length", strconv.Itoa(len(modified)))
		return nil
	}
}

const waitingPageHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Starting...</title>
<meta http-equiv="refresh" content="3">
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#0f1117;color:#e4e6ed;display:flex;align-items:center;justify-content:center;min-height:100vh}
.wrap{text-align:center}
.spinner{width:40px;height:40px;border:3px solid #2d3045;border-top-color:#6366f1;border-radius:50%;animation:spin .8s linear infinite;margin:0 auto 24px}
@keyframes spin{to{transform:rotate(360deg)}}
h2{font-size:1.25rem;margin-bottom:8px}
p{color:#8b8fa3;font-size:.875rem}
</style>
</head>
<body>
<div class="wrap">
<div class="spinner"></div>
<h2>Instance Starting</h2>
<p>OpenCode is initializing, this page will refresh automatically...</p>
</div>
</body>
</html>`
