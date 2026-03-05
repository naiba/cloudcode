package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

const (
	// InstTokenCookiePrefix is the per-instance token cookie name prefix.
	// Full cookie name: _cc_inst_token_{instanceID}
	InstTokenCookiePrefix = "_cc_inst_token_"
	// instTokenCookieMaxAge is how long the per-instance token cookie lives.
	instTokenCookieMaxAge = 86400 * 30 // 30 days
)

// instanceEntry holds per-instance proxy state.
type instanceEntry struct {
	stripProxy  *httputil.ReverseProxy
	directProxy *httputil.ReverseProxy
	token       string // per-instance access token (= OPENCODE_SERVER_PASSWORD)
}

// ReverseProxy manages dynamic reverse proxying to opencode instances.
type ReverseProxy struct {
	mu       sync.RWMutex
	entries  map[string]*instanceEntry // instanceID → entry
}

// New creates a new ReverseProxy manager.
func New() *ReverseProxy {
	return &ReverseProxy{
		entries: make(map[string]*instanceEntry),
	}
}

// Register adds or updates a proxy route for an instance.
// Traffic is routed directly to the container via the cloudcode-net Docker network.
// containerIP is the container's IP on cloudcode-net; port is the opencode listen port;
// token is the per-instance access token (used for both proxy validation and Basic Auth forwarding).
func (rp *ReverseProxy) Register(instanceID, containerIP string, port int, token string) error {
	target, err := url.Parse(fmt.Sprintf("http://%s:%d", containerIP, port))
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
		// Remove the CloudCode proxy auth header before forwarding — OpenCode
		// uses its own Basic Auth (OPENCODE_SERVER_PASSWORD) which is already
		// baked into the container env. We do NOT forward the client's
		// Authorization header; instead we inject the correct Basic Auth
		// credential so OpenCode accepts the proxied request.
		injectBasicAuth(req, token)
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
		req.Header.Del("Accept-Encoding")
		injectBasicAuth(req, token)
	}
	directProxy.ModifyResponse = injectInstanceIsolation(instanceID)
	directProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.entries[instanceID] = &instanceEntry{
		stripProxy:  stripProxy,
		directProxy: directProxy,
		token:       token,
	}

	return nil
}

// injectBasicAuth sets the Authorization header to Basic Auth with
// username "opencode" and the given password (the instance access token).
// This allows the CloudCode proxy to authenticate itself to the OpenCode
// server's native OPENCODE_SERVER_PASSWORD Basic Auth middleware.
func injectBasicAuth(req *http.Request, token string) {
	if token == "" {
		req.Header.Del("Authorization")
		return
	}
	creds := base64.StdEncoding.EncodeToString([]byte("opencode:" + token))
	req.Header.Set("Authorization", "Basic "+creds)
}

// Unregister removes a proxy route.
func (rp *ReverseProxy) Unregister(instanceID string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	delete(rp.entries, instanceID)
}

// ValidateToken checks whether the request carries a valid per-instance token.
// Accepted forms (in priority order):
//  1. Cookie _cc_inst_token_{id} (browser, set on first authenticated visit)
//  2. ?token= query parameter (browser first visit / redirect)
//  3. Authorization: Bearer <token> header (SDK / programmatic clients)
//
// Returns the token value if it matches, empty string otherwise.
func (rp *ReverseProxy) ValidateToken(r *http.Request, instanceID string) bool {
	rp.mu.RLock()
	entry, ok := rp.entries[instanceID]
	rp.mu.RUnlock()
	if !ok {
		return false
	}
	expected := entry.token
	if expected == "" {
		// No token set — allow (backwards compat / no-docker mode)
		return true
	}

	// 1. Cookie
	cookieName := InstTokenCookiePrefix + instanceID
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(expected)) == 1 {
			return true
		}
	}

	// 2. ?token= query param
	if t := r.URL.Query().Get("token"); t != "" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(expected)) == 1 {
			return true
		}
	}

	// 3. Authorization: Bearer <token>
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		t := strings.TrimPrefix(auth, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(t), []byte(expected)) == 1 {
			return true
		}
	}

	return false
}

// SetTokenCookie writes the per-instance token cookie to the response.
func SetTokenCookie(w http.ResponseWriter, r *http.Request, instanceID, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     InstTokenCookiePrefix + instanceID,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge: instTokenCookieMaxAge,
	})
}

// ServeHTTP handles proxied requests, stripping /instance/{id} prefix.
func (rp *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request, instanceID string) {
	rp.mu.RLock()
	entry, ok := rp.entries[instanceID]
	rp.mu.RUnlock()

	if !ok {
		http.Error(w, "Instance not found or not running", http.StatusBadGateway)
		return
	}

	entry.stripProxy.ServeHTTP(w, r)
}

// ServeHTTPDirect handles proxied requests, forwarding the original path as-is.
// Used for Referer-based fallback routing where the path is already correct
// (e.g. /assets/index-xxx.js, /global/health, WebSocket upgrades).
func (rp *ReverseProxy) ServeHTTPDirect(w http.ResponseWriter, r *http.Request, instanceID string) {
	rp.mu.RLock()
	entry, ok := rp.entries[instanceID]
	rp.mu.RUnlock()

	if !ok {
		http.Error(w, "Instance not found or not running", http.StatusBadGateway)
		return
	}

	entry.directProxy.ServeHTTP(w, r)
}

// IsRegistered checks if an instance has a registered proxy.
func (rp *ReverseProxy) IsRegistered(instanceID string) bool {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	_, ok := rp.entries[instanceID]
	return ok
}

func generateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func injectInstanceIsolation(instanceID string) func(*http.Response) error {
	idJSON, err := json.Marshal(instanceID)
	if err != nil {
		// instanceID is always a short alphanumeric string, this should never fail.
		// Fall back to a safe empty string to avoid injecting unescaped data.
		idJSON = []byte(`""`)
	}
	scriptBody := `
(function() {
  var K = "_cc_active_inst";
  var ID = ` + string(idJSON) + `;
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

  // Close old instance Web UI tabs when a new instance is opened
  if (typeof BroadcastChannel !== "undefined") {
    var ch = new BroadcastChannel("_cc_instance");
    ch.postMessage({ type: "activate", id: ID });
    ch.onmessage = function(e) {
      if (e.data && e.data.type === "activate" && e.data.id !== ID) {
        ch.close();
        if (window.opener || window.history.length <= 1) {
          window.close();
        }
        // window.close() may be blocked if not opened via script;
        // replace the page with a redirect to dashboard
        document.title = "Redirecting...";
        location.replace("/");
      }
    };
  }
})();
`

	const maxHTMLSize = 10 << 20 // 10 MB

	return func(resp *http.Response) error {
		// Strip CORS headers from the upstream OpenCode response.
		// OpenCode sets its own Access-Control-* headers; if we forward them
		// alongside our own the browser receives duplicate values and rejects
		// the response. We own the CORS policy at the proxy layer.
		resp.Header.Del("Access-Control-Allow-Origin")
		resp.Header.Del("Access-Control-Allow-Credentials")
		resp.Header.Del("Access-Control-Allow-Methods")
		resp.Header.Del("Access-Control-Allow-Headers")
		resp.Header.Del("Access-Control-Expose-Headers")
		resp.Header.Del("Access-Control-Max-Age")

		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			return nil
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLSize))
		resp.Body.Close()
		if err != nil {
			return err
		}

		// M3: find <head> case-insensitively without ToLower on the full body
		// (ToLower on non-ASCII UTF-8 can change byte lengths, corrupting the index).
		// Instead, search for all four case variants that matter in HTML.
		idx := indexCaseASCII(body, "<head>")
		if idx == -1 {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return nil
		}

		nonce := generateNonce()
		injection := []byte(`<script nonce="` + nonce + `">` + scriptBody + `</script>`)

		if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
			// M4: inject nonce into script-src if present, otherwise into default-src.
			if strings.Contains(csp, "script-src ") {
				csp = strings.Replace(csp, "script-src ", "script-src 'nonce-"+nonce+"' ", 1)
			} else if strings.Contains(csp, "default-src ") {
				csp = strings.Replace(csp, "default-src ", "default-src 'nonce-"+nonce+"' ", 1)
			}
			resp.Header.Set("Content-Security-Policy", csp)
		}

		insertAt := idx + len("<head>")
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

// indexCaseASCII finds the first occurrence of the ASCII tag (lowercase) in body,
// matching case-insensitively for ASCII letters only.  This avoids bytes.ToLower
// which can change byte lengths for non-ASCII UTF-8 content, corrupting the index.
func indexCaseASCII(body []byte, tag string) int {
	if len(tag) == 0 || len(body) < len(tag) {
		return -1
	}
	tagBytes := []byte(tag) // already lowercase
	for i := 0; i <= len(body)-len(tagBytes); i++ {
		match := true
		for j, tb := range tagBytes {
			cb := body[i+j]
			// ASCII lowercase
			if cb >= 'A' && cb <= 'Z' {
				cb += 'a' - 'A'
			}
			if cb != tb {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
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
