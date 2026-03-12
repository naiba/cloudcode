package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/vkenliu/cloudcode-docker/internal/config"
	"github.com/vkenliu/cloudcode-docker/internal/docker"
	"github.com/vkenliu/cloudcode-docker/internal/proxy"
	"github.com/vkenliu/cloudcode-docker/internal/store"
)

// instanceIDRe matches valid instance IDs: lowercase hex, exactly 8 chars.
var instanceIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// instanceNameRe mirrors the frontend pattern: letters, digits, hyphens, underscores, 1–64 chars.
var instanceNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// envVarKeyRe validates POSIX env var names: start with letter or underscore,
// followed by letters, digits, or underscores.
var envVarKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const sessionCookieName = "_cc_session"

// wsTokenEntry holds a one-time WebSocket auth token with its creation time.
type wsTokenEntry struct {
	createdAt time.Time
}

// sessionEntry holds a session creation time for expiry pruning.
type sessionEntry struct {
	createdAt time.Time
}

type Handler struct {
	store         *store.Store
	docker        *docker.Manager
	proxy         *proxy.ReverseProxy
	config        *config.Manager
	spaFS         fs.FS
	accessToken   string
	corsOrigins   []string      // allowed dev origins for WS CheckOrigin
	recyclingMu   sync.Mutex    // prevents concurrent enforceRecyclingPolicy runs
	sessions      sync.Map      // sessionID (string) → sessionEntry
	wsTokens      sync.Map      // one-time WS token (string) → wsTokenEntry
	loginAttempts sync.Map      // IP (string) → *loginState
	done          chan struct{} // closed on shutdown to stop background goroutines
}

// loginState tracks per-IP login rate limiting.
// mu guards count+resetAt together to prevent TOCTOU races.
type loginState struct {
	mu      sync.Mutex
	count   int32
	resetAt time.Time
}

// New creates a new Handler. spaFiles is an fs.FS rooted at the frontend dist
// directory (must contain index.html). Pass nil to disable SPA serving (returns
// 404 for all non-API routes).
func New(s *store.Store, dm *docker.Manager, rp *proxy.ReverseProxy, cfgMgr *config.Manager, spaFiles fs.FS, accessToken string, corsOrigins []string) *Handler {
	h := &Handler{
		store:       s,
		docker:      dm,
		proxy:       rp,
		config:      cfgMgr,
		spaFS:       spaFiles,
		accessToken: accessToken,
		corsOrigins: corsOrigins,
		done:        make(chan struct{}),
	}

	// Re-register running instances into the proxy on startup.
	// Use GetContainerIPAndPort to correctly handle legacy containers that
	// may listen on a non-default port (created before port-pool removal).
	if dm != nil {
		instances, err := s.List()
		if err == nil {
			for _, inst := range instances {
				if inst.Status == "running" && inst.ContainerID != "" {
					ip, port, err := dm.GetContainerIPAndPort(context.Background(), inst.ContainerID)
					if err != nil {
						log.Printf("Warning: could not get IP/port for instance %s: %v", inst.ID, err)
						continue
					}
					if err := rp.Register(inst.ID, ip, port, inst.AccessToken); err != nil {
						log.Printf("Warning: could not register proxy for instance %s: %v", inst.ID, err)
					}
				}
			}
		}
	}

	// Background goroutines: prune expired entries periodically.
	go h.pruneWSTokens()
	go h.pruneLoginAttempts()
	go h.pruneSessions()

	return h
}

// Shutdown signals background goroutines to stop. Call on graceful shutdown.
func (h *Handler) Shutdown() {
	close(h.done)
}

const (
	wsTokenTTL    = 60 * time.Second
	sessionTTL    = 30 * 24 * time.Hour // match cookie MaxAge
	loginEntryTTL = 5 * time.Minute     // evict IP state after 5 min of inactivity
)

// pruneWSTokens periodically removes WS tokens older than wsTokenTTL.
func (h *Handler) pruneWSTokens() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			now := time.Now()
			h.wsTokens.Range(func(k, v any) bool {
				if e, ok := v.(wsTokenEntry); ok && now.Sub(e.createdAt) > wsTokenTTL {
					h.wsTokens.Delete(k)
				}
				return true
			})
		}
	}
}

// pruneLoginAttempts periodically evicts stale per-IP rate-limit entries.
func (h *Handler) pruneLoginAttempts() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			now := time.Now()
			h.loginAttempts.Range(func(k, v any) bool {
				if ls, ok := v.(*loginState); ok {
					ls.mu.Lock()
					idle := now.Sub(ls.resetAt) > loginEntryTTL
					ls.mu.Unlock()
					if idle {
						h.loginAttempts.Delete(k)
					}
				}
				return true
			})
		}
	}
}

// pruneSessions periodically removes expired platform sessions.
func (h *Handler) pruneSessions() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			now := time.Now()
			h.sessions.Range(func(k, v any) bool {
				if e, ok := v.(sessionEntry); ok && now.Sub(e.createdAt) > sessionTTL {
					h.sessions.Delete(k)
				}
				return true
			})
		}
	}
}

// RegisterRoutes sets up all HTTP routes.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// --- Auth routes (public — no session required) ---
	mux.HandleFunc("POST /api/auth/login", h.apiAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", h.apiAuthLogout)
	// WS token: protected (requires session), used by browser to get a one-time
	// token it can pass as ?token= on cross-origin WebSocket connections.
	mux.Handle("GET /api/auth/ws-token", h.auth(http.HandlerFunc(h.apiAuthWSToken)))

	// --- Instances API (protected) ---
	mux.Handle("GET /api/instances", h.auth(http.HandlerFunc(h.apiListInstances)))
	mux.Handle("POST /api/instances", h.auth(http.HandlerFunc(h.apiCreateInstance)))
	mux.Handle("GET /api/instances/disk-usage", h.auth(http.HandlerFunc(h.apiDiskUsage)))
	mux.Handle("GET /api/instances/{id}", h.auth(http.HandlerFunc(h.apiGetInstance)))
	mux.Handle("DELETE /api/instances/{id}", h.auth(http.HandlerFunc(h.apiDeleteInstance)))
	mux.Handle("POST /api/instances/{id}/start", h.auth(http.HandlerFunc(h.apiStartInstance)))
	mux.Handle("POST /api/instances/{id}/stop", h.auth(http.HandlerFunc(h.apiStopInstance)))
	mux.Handle("POST /api/instances/{id}/restart", h.auth(http.HandlerFunc(h.apiRestartInstance)))
	mux.Handle("GET /api/instances/{id}/status", h.auth(http.HandlerFunc(h.apiInstanceStatus)))
	mux.Handle("POST /api/instances/{id}/regenerate-token", h.auth(http.HandlerFunc(h.apiRegenerateToken)))
	mux.Handle("PATCH /api/instances/{id}/env-vars", h.auth(http.HandlerFunc(h.apiUpdateInstanceEnvVars)))

	// --- Batch status (protected) ---
	mux.Handle("POST /api/status/instances", h.auth(http.HandlerFunc(h.apiBatchInstanceStatus)))

	// --- System API (protected) ---
	mux.Handle("GET /api/system/resources", h.auth(http.HandlerFunc(h.apiSystemResources)))

	// --- Settings API (protected) ---
	mux.Handle("GET /api/settings", h.auth(http.HandlerFunc(h.apiGetSettings)))
	mux.Handle("PUT /api/settings/env", h.auth(http.HandlerFunc(h.apiSaveEnvVars)))
	mux.Handle("PUT /api/settings/startup-script", h.auth(http.HandlerFunc(h.apiSaveStartupScript)))
	mux.Handle("GET /api/settings/file", h.auth(http.HandlerFunc(h.apiGetConfigFile)))
	mux.Handle("PUT /api/settings/file", h.auth(http.HandlerFunc(h.apiSaveConfigFile)))
	mux.Handle("GET /api/settings/dir-files", h.auth(http.HandlerFunc(h.apiListDirFiles)))
	mux.Handle("PUT /api/settings/dir-file", h.auth(http.HandlerFunc(h.apiSaveDirFile)))
	mux.Handle("DELETE /api/settings/dir-file", h.auth(http.HandlerFunc(h.apiDeleteDirFile)))
	mux.Handle("DELETE /api/settings/agents-skill", h.auth(http.HandlerFunc(h.apiDeleteAgentsSkill)))
	mux.Handle("PUT /api/settings/cors", h.auth(http.HandlerFunc(h.apiSaveCORSOrigins)))
	mux.Handle("PUT /api/settings/recycling", h.auth(http.HandlerFunc(h.apiSaveRecyclingPolicy)))

	// --- WebSocket endpoints (protected via cookie OR one-time ?token=) ---
	mux.HandleFunc("GET /instances/{id}/logs/ws", h.handleLogsWS)
	mux.HandleFunc("GET /instances/{id}/terminal/ws", h.handleTerminalWS)

	// --- Reverse proxy to OpenCode web UI ---
	// No platform session required — handleProxy validates the per-instance
	// access token (cookie, ?token=, or Authorization: Bearer) itself.
	mux.HandleFunc("/instance/{id}/", h.handleProxy)

	// --- Catch-all: SPA asset fallback / proxy fallback (must be last) ---
	// Public for /login route (SPA handles it); protected for all other paths.
	mux.HandleFunc("/", h.handleCatchAll)
}

// --- Auth helpers ---

// newSessionID generates a cryptographically random 32-byte hex session ID.
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// auth is the authentication middleware. It checks for a valid session cookie.
// For /api/* paths it returns 401 JSON on failure; for browser paths it redirects to /login.
func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.isAuthenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/instances/") {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// isAuthenticated returns true if the request carries a valid, non-expired session cookie.
func (h *Handler) isAuthenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	v, ok := h.sessions.Load(c.Value)
	if !ok {
		return false
	}
	// Enforce TTL even if the prune goroutine hasn't run yet.
	e, ok := v.(sessionEntry)
	return ok && time.Since(e.createdAt) <= sessionTTL
}

// isAuthenticatedWS returns true for WebSocket upgrade requests.
// It accepts either the session cookie OR a one-time ?token= query parameter
// (consumed on first use) to support cross-origin WS from the dev frontend.
func (h *Handler) isAuthenticatedWS(r *http.Request) bool {
	if h.isAuthenticated(r) {
		return true
	}
	return h.consumeWSToken(r.URL.Query().Get("token"))
}

// setSessionCookie writes the session cookie to the response.
func (h *Handler) setSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 30, // 30 days
	})
}

// clearSessionCookie expires the session cookie.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// --- Auth API handlers ---

// loginRateLimit allows 10 attempts per IP per 60 s window.
const (
	loginMaxAttempts = 10
	loginWindow      = 60 * time.Second
)

func (h *Handler) loginAllowed(ip string) bool {
	raw, _ := h.loginAttempts.LoadOrStore(ip, &loginState{})
	ls := raw.(*loginState)

	ls.mu.Lock()
	defer ls.mu.Unlock()

	now := time.Now()
	if now.After(ls.resetAt) {
		// Window expired — reset atomically under the lock.
		ls.count = 0
		ls.resetAt = now.Add(loginWindow)
	}
	ls.count++
	return ls.count <= loginMaxAttempts
}

func (h *Handler) apiAuthLogin(w http.ResponseWriter, r *http.Request) {
	// H1: per-IP rate limiting
	ip := r.RemoteAddr
	if i := strings.LastIndex(ip, ":"); i != -1 {
		ip = ip[:i]
	}
	if !h.loginAllowed(ip) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts, try again later")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Constant-time comparison to prevent timing attacks.
	tokenMatch := subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.accessToken)) == 1
	if !tokenMatch {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	// C3: invalidate any existing session for this browser before issuing a new one.
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		h.sessions.Delete(c.Value)
	}

	sessionID, err := newSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	h.sessions.Store(sessionID, sessionEntry{createdAt: time.Now()})
	h.setSessionCookie(w, r, sessionID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) apiAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		h.sessions.Delete(c.Value)
	}
	clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// apiAuthWSToken issues a single-use token for WebSocket authentication.
// The browser fetches this (with its session cookie, via the same-origin proxy)
// and passes it as ?token= on the cross-origin WebSocket URL to the Go backend.
// Tokens are consumed on first use and never reused.
func (h *Handler) apiAuthWSToken(w http.ResponseWriter, r *http.Request) {
	token, err := newSessionID() // reuse same CSPRNG helper
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	h.wsTokens.Store(token, wsTokenEntry{createdAt: time.Now()})
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// consumeWSToken checks and atomically removes a one-time WS token.
// Returns false if the token is missing or has expired.
func (h *Handler) consumeWSToken(token string) bool {
	if token == "" {
		return false
	}
	v, ok := h.wsTokens.LoadAndDelete(token)
	if !ok {
		return false
	}
	e, ok := v.(wsTokenEntry)
	return ok && time.Since(e.createdAt) <= wsTokenTTL
}

// instanceResponse is the API response shape for a store.Instance.
// AccessToken is included so authenticated users can retrieve the token for
// SDK access or to pass to opencode attach.
type instanceResponse struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	ContainerID    string            `json:"container_id"`
	Status         string            `json:"status"`
	ErrorMsg       string            `json:"error_msg"`
	WorkDir        string            `json:"work_dir"`
	MemoryMB       int               `json:"memory_mb"`
	CPUCores       float64           `json:"cpu_cores"`
	EnvVars        map[string]string `json:"env_vars"`
	AccessToken    string            `json:"access_token"`
	DiskUsageBytes *int64            `json:"disk_usage_bytes,omitempty"` // nil = not fetched, -1 = unavailable
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
}

func toInstanceResponse(inst *store.Instance) instanceResponse {
	return instanceResponse{
		ID:          inst.ID,
		Name:        inst.Name,
		ContainerID: inst.ContainerID,
		Status:      inst.Status,
		ErrorMsg:    inst.ErrorMsg,
		WorkDir:     inst.WorkDir,
		MemoryMB:    inst.MemoryMB,
		CPUCores:    inst.CPUCores,
		EnvVars:     inst.EnvVars,
		AccessToken: inst.AccessToken,
		CreatedAt:   inst.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   inst.UpdatedAt.Format(time.RFC3339),
	}
}

func toInstanceResponses(instances []*store.Instance) []instanceResponse {
	out := make([]instanceResponse, len(instances))
	for i, inst := range instances {
		out[i] = toInstanceResponse(inst)
	}
	return out
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// --- Instance API handlers ---

func (h *Handler) apiListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := h.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list instances")
		return
	}
	// Sync Docker status for each instance in parallel (#15)
	if h.docker != nil {
		var wg sync.WaitGroup
		for _, inst := range instances {
			if inst.ContainerID == "" {
				continue
			}
			wg.Add(1)
			go func(inst *store.Instance) {
				defer wg.Done()
				if status, err := h.docker.ContainerStatus(r.Context(), inst.ContainerID); err == nil && status != inst.Status {
					inst.Status = status
					if err := h.store.Update(inst); err != nil {
						log.Printf("Warning: failed to update instance %s status: %v", inst.ID, err)
					}
				}
			}(inst)
		}
		wg.Wait()
	}
	if instances == nil {
		instances = []*store.Instance{}
	}
	writeJSON(w, http.StatusOK, toInstanceResponses(instances))
}

func (h *Handler) apiGetInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if inst.ContainerID != "" && h.docker != nil {
		if status, err := h.docker.ContainerStatus(r.Context(), inst.ContainerID); err == nil {
			inst.Status = status
			_ = h.store.Update(inst)
		}
	}
	resp := toInstanceResponse(inst)
	// Attach disk usage from cached volume data.
	if h.docker != nil {
		if usage, err := h.docker.VolumeDiskUsage(r.Context()); err == nil {
			if size, ok := usage[inst.ID]; ok {
				resp.DiskUsageBytes = &size
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) apiCreateInstance(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit (#17)
	var req struct {
		Name     string            `json:"name"`
		MemoryMB int               `json:"memory_mb"`
		CPUCores float64           `json:"cpu_cores"`
		EnvVars  map[string]string `json:"env_vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !instanceNameRe.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be 1–64 characters: letters, digits, hyphens, underscores only")
		return
	}

	if existing, _ := h.store.GetByName(req.Name); existing != nil {
		writeError(w, http.StatusConflict, "instance name already exists")
		return
	}

	// Generate a per-instance access token (used for both proxy auth and
	// OPENCODE_SERVER_PASSWORD Basic Auth inside the container).
	accessToken, err := newSessionID() // reuse same CSPRNG helper (32-byte hex)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate access token")
		return
	}

	// Validate and sanitise per-instance env vars.
	// Keys must be valid POSIX env var names; empty keys/values are rejected.
	envVars := make(map[string]string)
	for k, v := range req.EnvVars {
		if !envVarKeyRe.MatchString(k) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid env var key %q: must match [A-Za-z_][A-Za-z0-9_]*", k))
			return
		}
		envVars[k] = v
	}

	inst := &store.Instance{
		Name:        req.Name,
		Status:      "created",
		Port:        docker.ContainerPort(),
		WorkDir:     "/root",
		EnvVars:     envVars,
		MemoryMB:    req.MemoryMB,
		CPUCores:    req.CPUCores,
		AccessToken: accessToken,
	}

	// #13: retry on ID collision (astronomically rare but correct to handle)
	var createErr error
	for attempt := 0; attempt < 5; attempt++ {
		inst.ID = uuid.New().String()[:8]
		if createErr = h.store.Create(inst); createErr == nil {
			break
		}
		if !strings.Contains(createErr.Error(), "UNIQUE constraint") {
			break
		}
	}
	if createErr != nil {
		writeError(w, http.StatusInternalServerError, "failed to create instance")
		return
	}

	if h.docker != nil {
		containerID, err := h.docker.CreateContainer(r.Context(), inst) // #7 use r.Context()
		if err != nil {
			log.Printf("Error creating container for %s: %v", inst.ID, err)
			inst.Status = "error"
			inst.ErrorMsg = err.Error()
			if updateErr := h.store.Update(inst); updateErr != nil {
				log.Printf("Warning: failed to update instance %s: %v", inst.ID, updateErr)
			}
		} else {
			inst.ContainerID = containerID
			inst.Status = "running"
			if updateErr := h.store.Update(inst); updateErr != nil {
				log.Printf("Warning: failed to update instance %s: %v", inst.ID, updateErr)
			}
			// Get container IP/port and register with the proxy. Failure is fatal
			// here: return an error so the user knows the instance is unreachable
			// rather than silently marking it running with a perpetual 502.
			ip, port, err := h.docker.GetContainerIPAndPort(r.Context(), containerID)
			if err != nil {
				inst.Status = "error"
				inst.ErrorMsg = "could not get container address: " + err.Error()
				_ = h.store.Update(inst)
				writeError(w, http.StatusInternalServerError, inst.ErrorMsg)
				return
			}
			if err := h.proxy.Register(inst.ID, ip, port, inst.AccessToken); err != nil {
				inst.Status = "error"
				inst.ErrorMsg = "could not register proxy: " + err.Error()
				_ = h.store.Update(inst)
				writeError(w, http.StatusInternalServerError, inst.ErrorMsg)
				return
			}
		}
	}

	// Invalidate disk cache so the new volume appears in usage queries.
	if h.docker != nil {
		h.docker.InvalidateDiskCache()
	}

	writeJSON(w, http.StatusCreated, toInstanceResponse(inst))
}

func (h *Handler) apiDeleteInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	if inst.ContainerID != "" && h.docker != nil {
		// Use a background context so the removal completes even if the HTTP
		// client disconnects — we don't want orphaned containers or volumes.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.docker.RemoveContainerAndVolume(ctx, inst.ContainerID, id); err != nil {
			log.Printf("Error removing container/volume for %s: %v", id, err)
		}
	}

	h.proxy.Unregister(id)
	h.config.RemoveInstanceData(id)

	if err := h.store.Delete(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete instance")
		return
	}

	// L4: clear the instance routing cookie so the browser stops sending stale
	// _cc_inst values for this deleted instance.
	http.SetCookie(w, &http.Cookie{
		Name:     instanceCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	writeNoContent(w)
}

// enforceRecyclingPolicy checks the recycling policy and removes the oldest
// inactive (stopped/exited/error) instances if they exceed the configured
// maximum. Runs in the background so it does not block the caller.
// recyclingMu prevents concurrent runs from racing on the same instances.
func (h *Handler) enforceRecyclingPolicy() {
	go func() {
		// Serialize recycling runs to prevent TOCTOU races where concurrent
		// goroutines read the same list and try to delete the same instances.
		h.recyclingMu.Lock()
		defer h.recyclingMu.Unlock()

		policy, err := h.config.GetRecyclingPolicy()
		if err != nil || !policy.Enabled {
			return
		}

		instances, err := h.store.List()
		if err != nil {
			log.Printf("Warning: recycling policy: failed to list instances: %v", err)
			return
		}

		// Collect inactive instances (stopped, exited, error), ordered by
		// created_at DESC (newest first) because store.List returns that order.
		var inactive []*store.Instance
		for _, inst := range instances {
			if inst.Status == "stopped" || inst.Status == "exited" || inst.Status == "error" {
				inactive = append(inactive, inst)
			}
		}

		if len(inactive) <= policy.MaxStoppedCount {
			return
		}

		// Remove the oldest ones (tail of the list since list is newest-first).
		toRemove := inactive[policy.MaxStoppedCount:]
		for _, inst := range toRemove {
			log.Printf("Recycling policy: removing inactive instance %s (%s, status=%s)", inst.ID, inst.Name, inst.Status)

			if inst.ContainerID != "" && h.docker != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := h.docker.RemoveContainerAndVolume(ctx, inst.ContainerID, inst.ID); err != nil {
					log.Printf("Warning: recycling: failed to remove container for %s: %v", inst.ID, err)
				}
				cancel()
			}

			h.proxy.Unregister(inst.ID)
			h.config.RemoveInstanceData(inst.ID)

			if err := h.store.Delete(inst.ID); err != nil {
				log.Printf("Warning: recycling: failed to delete instance %s: %v", inst.ID, err)
			}
		}

		// Invalidate disk cache since we removed volumes.
		if h.docker != nil {
			h.docker.InvalidateDiskCache()
		}

		log.Printf("Recycling policy: removed %d inactive instance(s)", len(toRemove))
	}()
}

func (h *Handler) apiStartInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	if h.docker == nil {
		writeError(w, http.StatusInternalServerError, "docker is not available")
		return
	}

	if inst.ContainerID == "" {
		containerID, err := h.docker.CreateContainer(r.Context(), inst) // #7
		if err != nil {
			inst.Status = "error"
			inst.ErrorMsg = err.Error()
			_ = h.store.Update(inst)
			writeError(w, http.StatusInternalServerError, "failed to create container: "+err.Error())
			return
		}
		inst.ContainerID = containerID
	} else {
		if err := h.docker.StartContainer(r.Context(), inst.ContainerID); err != nil { // #7
			inst.Status = "error"
			inst.ErrorMsg = err.Error()
			_ = h.store.Update(inst)
			writeError(w, http.StatusInternalServerError, "failed to start container: "+err.Error())
			return
		}
	}

	inst.Status = "running"
	inst.ErrorMsg = ""
	if err := h.store.Update(inst); err != nil { // #9
		log.Printf("Warning: failed to update instance %s: %v", inst.ID, err)
	}
	if ip, port, err := h.docker.GetContainerIPAndPort(r.Context(), inst.ContainerID); err != nil {
		log.Printf("Warning: could not get IP for instance %s: %v", inst.ID, err)
	} else if err := h.proxy.Register(inst.ID, ip, port, inst.AccessToken); err != nil { // #9
		log.Printf("Warning: failed to register proxy for %s: %v", inst.ID, err)
	}

	writeJSON(w, http.StatusOK, toInstanceResponse(inst))
}

func (h *Handler) apiStopInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	if inst.ContainerID != "" && h.docker != nil {
		if err := h.docker.StopContainer(r.Context(), inst.ContainerID); err != nil { // #7
			writeError(w, http.StatusInternalServerError, "failed to stop container: "+err.Error())
			return
		}
	}

	inst.Status = "stopped"
	if err := h.store.Update(inst); err != nil { // #9
		log.Printf("Warning: failed to update instance %s: %v", inst.ID, err)
	}
	h.proxy.Unregister(id)

	// Check recycling policy in the background.
	h.enforceRecyclingPolicy()

	writeJSON(w, http.StatusOK, toInstanceResponse(inst))
}

func (h *Handler) apiRestartInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	if h.docker == nil {
		writeError(w, http.StatusInternalServerError, "docker is not available")
		return
	}

	if inst.ContainerID != "" {
		// #24: log stop/remove errors instead of silently ignoring
		if err := h.docker.StopContainer(r.Context(), inst.ContainerID); err != nil { // #7
			log.Printf("Warning: failed to stop container %s during restart: %v", inst.ContainerID, err)
		}
		if err := h.docker.RemoveContainer(r.Context(), inst.ContainerID); err != nil { // #7, #24
			log.Printf("Warning: failed to remove container %s during restart: %v", inst.ContainerID, err)
		}
	}

	containerID, err := h.docker.CreateContainer(r.Context(), inst) // #7
	if err != nil {
		inst.Status = "error"
		inst.ErrorMsg = err.Error()
		if updateErr := h.store.Update(inst); updateErr != nil { // #9
			log.Printf("Warning: failed to update instance %s: %v", inst.ID, updateErr)
		}
		writeError(w, http.StatusInternalServerError, "failed to restart container: "+err.Error())
		return
	}

	inst.ContainerID = containerID
	inst.Status = "running"
	if err := h.store.Update(inst); err != nil { // #9
		log.Printf("Warning: failed to update instance %s: %v", inst.ID, err)
	}
	if ip, port, err := h.docker.GetContainerIPAndPort(r.Context(), containerID); err != nil {
		log.Printf("Warning: could not get IP for instance %s: %v", inst.ID, err)
	} else if err := h.proxy.Register(inst.ID, ip, port, inst.AccessToken); err != nil { // #9
		log.Printf("Warning: failed to register proxy for %s: %v", inst.ID, err)
	}

	writeJSON(w, http.StatusOK, toInstanceResponse(inst))
}

func (h *Handler) apiInstanceStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		// Instance deleted — return empty 200 so frontend removes it
		w.WriteHeader(http.StatusOK)
		return
	}

	clientStatus := r.URL.Query().Get("s")
	if inst.ContainerID != "" && h.docker != nil {
		if status, err := h.docker.ContainerStatus(r.Context(), inst.ContainerID); err == nil && status != inst.Status {
			inst.Status = status
			_ = h.store.Update(inst)
		}
	}

	if inst.Status == clientStatus {
		writeNoContent(w)
		return
	}
	writeJSON(w, http.StatusOK, toInstanceResponse(inst))
}

// apiBatchInstanceStatus checks multiple instances at once and returns only
// those whose status differs from the client's known status, plus any that
// have been deleted (returned as {"id": null}).
//
// Request:  POST /api/instances/status
//
//	{"ids": {"<id>": "<clientStatus>", ...}}
//
// Response: 200 {"changed": {"<id>": <Instance|null>, ...}}
//
//	Only IDs whose status changed (or were deleted) appear in the response.
func (h *Handler) apiBatchInstanceStatus(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		IDs map[string]string `json:"ids"` // id → clientStatus
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.IDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"changed": map[string]any{}})
		return
	}
	// L5: guard against unbounded goroutine spawning.
	const maxBatchIDs = 500
	if len(req.IDs) > maxBatchIDs {
		writeError(w, http.StatusBadRequest, "too many IDs in batch request")
		return
	}

	type result struct {
		id      string
		inst    *store.Instance // nil means deleted
		changed bool
	}

	results := make(chan result, len(req.IDs))
	var wg sync.WaitGroup

	for id, clientStatus := range req.IDs {
		wg.Add(1)
		go func(id, clientStatus string) {
			defer wg.Done()
			inst, err := h.store.Get(id)
			if err != nil {
				// Not found → deleted
				results <- result{id: id, inst: nil, changed: true}
				return
			}
			// Sync with Docker if possible
			if inst.ContainerID != "" && h.docker != nil {
				if status, err := h.docker.ContainerStatus(r.Context(), inst.ContainerID); err == nil && status != inst.Status {
					inst.Status = status
					if updateErr := h.store.Update(inst); updateErr != nil {
						log.Printf("Warning: failed to update instance %s status: %v", inst.ID, updateErr)
					}
				}
			}
			if inst.Status != clientStatus {
				results <- result{id: id, inst: inst, changed: true}
			} else {
				results <- result{id: id, inst: inst, changed: false}
			}
		}(id, clientStatus)
	}

	wg.Wait()
	close(results)

	changed := map[string]any{}
	hasNewStopped := false
	for res := range results {
		if !res.changed {
			continue
		}
		if res.inst == nil {
			changed[res.id] = nil
		} else {
			r := toInstanceResponse(res.inst)
			changed[res.id] = r
			if res.inst.Status == "stopped" || res.inst.Status == "exited" || res.inst.Status == "error" {
				hasNewStopped = true
			}
		}
	}

	// Trigger recycling if any instance transitioned to an inactive state.
	if hasNewStopped {
		h.enforceRecyclingPolicy()
	}

	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

// apiUpdateInstanceEnvVars replaces the per-instance env vars with the provided map.
// The new vars are persisted to the DB immediately. The container must be restarted
// for the changes to take effect (env vars are injected at container creation time).
func (h *Handler) apiUpdateInstanceEnvVars(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		EnvVars map[string]string `json:"env_vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate keys
	envVars := make(map[string]string, len(req.EnvVars))
	for k, v := range req.EnvVars {
		if !envVarKeyRe.MatchString(k) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid env var key %q: must match [A-Za-z_][A-Za-z0-9_]*", k))
			return
		}
		envVars[k] = v
	}

	inst.EnvVars = envVars
	if err := h.store.Update(inst); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update env vars")
		return
	}

	writeJSON(w, http.StatusOK, toInstanceResponse(inst))
}

// apiRegenerateToken generates a new access token for an instance.
// The new token is stored in the DB, the proxy registration is updated, and the
// new token is returned in the response. The instance must be authenticated with
// the platform session (standard auth middleware applies).
func (h *Handler) apiRegenerateToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}

	newToken, err := newSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	inst.AccessToken = newToken
	if err := h.store.Update(inst); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update token")
		return
	}

	// Update the proxy with the new token so future requests are validated correctly.
	// Note: existing containers still have the old OPENCODE_SERVER_PASSWORD — a restart
	// is required to propagate the new token to OpenCode's Basic Auth middleware.
	if h.docker != nil && inst.ContainerID != "" {
		if ip, port, err := h.docker.GetContainerIPAndPort(r.Context(), inst.ContainerID); err != nil {
			log.Printf("Warning: could not get IP for instance %s on token regeneration: %v", id, err)
		} else {
			_ = h.proxy.Register(id, ip, port, newToken)
		}
	}

	// L4: clear the old per-instance token cookie so the browser is forced to
	// re-authenticate with the new token via ?token= on the next visit.
	http.SetCookie(w, &http.Cookie{
		Name:     proxy.InstTokenCookiePrefix + id,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	writeJSON(w, http.StatusOK, map[string]string{"access_token": newToken})
}

// --- System API ---

func (h *Handler) apiSystemResources(w http.ResponseWriter, r *http.Request) {
	totalMemMB := hostMemoryMB()
	writeJSON(w, http.StatusOK, map[string]int{
		"total_memory_mb": totalMemMB,
		"total_cpu_cores": runtime.NumCPU(),
	})
}

// apiDiskUsage returns a map of instanceID → disk usage bytes for all instances.
// Uses the Docker volume cache (1-hour TTL) to avoid expensive system-df calls.
func (h *Handler) apiDiskUsage(w http.ResponseWriter, r *http.Request) {
	if h.docker == nil {
		writeJSON(w, http.StatusOK, map[string]int64{})
		return
	}
	usage, err := h.docker.VolumeDiskUsage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get disk usage: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// --- Settings API ---

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	// Env vars as ordered array (#21: log error, don't silently return empty)
	envMap, envErr := h.config.GetEnvVars()
	if envErr != nil {
		log.Printf("Warning: failed to get env vars: %v", envErr)
		envMap = map[string]string{}
	}
	type envVar struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	envVars := []envVar{}
	for k, v := range envMap {
		envVars = append(envVars, envVar{Key: k, Value: v})
	}

	// Config files with content.
	// H5: auth.json content is excluded from the default list response — it
	// is only served by the dedicated GET /api/settings/file endpoint which
	// requires an explicit user action.
	type configFile struct {
		Name     string  `json:"name"`
		RelPath  string  `json:"rel_path"`
		Hint     string  `json:"hint"`
		Content  *string `json:"content"` // nil means "load on demand"
		ReadOnly bool    `json:"read_only,omitempty"`
	}
	const authRelPath = "opencode-data/auth.json"
	var configFiles []configFile
	for _, f := range h.config.EditableFiles() {
		var contentPtr *string
		if filepath.ToSlash(f.RelPath) != authRelPath {
			c, _ := h.config.ReadFile(f.RelPath)
			contentPtr = &c
		}
		configFiles = append(configFiles, configFile{
			Name:    f.Name,
			RelPath: f.RelPath,
			Hint:    f.Hint,
			Content: contentPtr,
		})
	}

	// Dir files
	type dirFile struct {
		Name    string `json:"name"`
		RelPath string `json:"rel_path"`
	}
	dirNames := []string{"commands", "agents", "skills", "plugins"}
	dirs := map[string][]dirFile{}
	for _, d := range dirNames {
		files, _ := h.config.ListDirFiles(d)
		arr := []dirFile{}
		for _, f := range files {
			arr = append(arr, dirFile{Name: f.Name, RelPath: f.RelPath})
		}
		dirs[d] = arr
	}

	// Agents skills
	type agentsSkill struct {
		SkillName string `json:"skill_name"`
		RelPath   string `json:"rel_path"`
	}
	rawSkills, _ := h.config.ListAgentsSkills()
	agentsSkills := []agentsSkill{}
	for _, s := range rawSkills {
		agentsSkills = append(agentsSkills, agentsSkill{SkillName: s.SkillName, RelPath: s.RelPath})
	}

	// Directory mappings
	configDir := h.config.RootDir()
	type dirMapping struct {
		Host      string `json:"host"`
		Container string `json:"container"`
	}
	mappings := []dirMapping{
		{Host: filepath.Join(configDir, "opencode") + "/", Container: "/root/.config/opencode/"},
		{Host: filepath.Join(configDir, "opencode-data", "auth.json"), Container: "/root/.local/share/opencode/auth.json"},
		{Host: filepath.Join(configDir, "dot-opencode") + "/", Container: "/root/.opencode/"},
		{Host: filepath.Join(configDir, "agents-skills") + "/", Container: "/root/.agents/"},
		{Host: filepath.Join(configDir, "adit-core") + "/", Container: "/root/.adit-core/"},
	}

	// Startup script
	startupScript, _ := h.config.GetStartupScript()

	// CORS origins
	corsOrigins, _ := h.config.GetCORSOrigins()
	if corsOrigins == nil {
		corsOrigins = []string{}
	}

	// Recycling policy
	recyclingPolicy, _ := h.config.GetRecyclingPolicy()

	writeJSON(w, http.StatusOK, map[string]any{
		"config_dir":         configDir,
		"env_vars":           envVars,
		"config_files":       configFiles,
		"dirs":               dirs,
		"agents_skills":      agentsSkills,
		"directory_mappings": mappings,
		"startup_script":     startupScript,
		"cors_origins":       corsOrigins,
		"recycling_policy":   recyclingPolicy,
	})
}

func (h *Handler) apiSaveEnvVars(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit (#17)
	var req struct {
		Vars []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	env := make(map[string]string)
	for _, v := range req.Vars {
		k := strings.TrimSpace(v.Key)
		if k == "" {
			continue
		}
		env[k] = v.Value // #14: preserve value as-is, do not trim spaces
	}

	if err := h.config.SetEnvVars(env); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save environment variables: "+err.Error())
		return
	}
	writeNoContent(w)
}

func (h *Handler) apiSaveStartupScript(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
	var req struct {
		Script string `json:"script"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.config.SetStartupScript(req.Script); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save startup script: "+err.Error())
		return
	}
	writeNoContent(w)
}

func (h *Handler) apiSaveCORSOrigins(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
	var req struct {
		Origins []string `json:"origins"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Validate, deduplicate, and trim whitespace.
	seen := make(map[string]bool)
	var clean []string
	for _, o := range req.Origins {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		// Validate origin format: must be scheme://host[:port] with no path.
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid origin %q: must be scheme://host[:port]", o))
			return
		}
		if u.Path != "" && u.Path != "/" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid origin %q: must not contain a path", o))
			return
		}
		// Normalize: scheme://host (strip trailing slash)
		normalized := u.Scheme + "://" + u.Host
		if !seen[strings.ToLower(normalized)] {
			seen[strings.ToLower(normalized)] = true
			clean = append(clean, normalized)
		}
	}
	if clean == nil {
		clean = []string{}
	}
	if err := h.config.SetCORSOrigins(clean); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save CORS origins: "+err.Error())
		return
	}
	log.Printf("CORS origins updated: %v", clean)
	writeNoContent(w)
}

func (h *Handler) apiSaveRecyclingPolicy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Enabled         bool `json:"enabled"`
		MaxStoppedCount int  `json:"max_stopped_count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	policy := config.RecyclingPolicy{
		Enabled:         req.Enabled,
		MaxStoppedCount: req.MaxStoppedCount,
	}
	if err := h.config.SetRecyclingPolicy(policy); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save recycling policy: "+err.Error())
		return
	}
	log.Printf("Recycling policy updated: enabled=%v max_stopped=%d", policy.Enabled, policy.MaxStoppedCount)

	// Enforce immediately if just enabled.
	if policy.Enabled {
		h.enforceRecyclingPolicy()
	}

	writeNoContent(w)
}

// authJSONRelPath is the relative path for auth.json (normalised to forward slashes).
const authJSONRelPath = "opencode-data/auth.json"

func (h *Handler) apiGetConfigFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	// M14: auth.json may only be read via this endpoint by an explicit request,
	// but block direct ?path= access to prevent casual leakage of OAuth tokens.
	// Authenticated admins who need to edit auth.json use the Settings UI which
	// calls this endpoint with the correct rel_path from the EditableFiles list.
	// We allow it — the user deliberately requested it — but log the access.
	if filepath.ToSlash(relPath) == authJSONRelPath {
		log.Printf("Info: auth.json read via /api/settings/file by %s", r.RemoteAddr)
	}
	content, err := h.config.ReadFile(relPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read file: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"rel_path": relPath,
		"content":  content,
	})
}

func (h *Handler) apiSaveConfigFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10 MB limit (#17)
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := h.config.WriteFile(req.Path, req.Content); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save file: "+err.Error())
		return
	}
	writeNoContent(w)
}

var validDirNames = map[string]bool{
	"commands": true,
	"agents":   true,
	"skills":   true,
	"plugins":  true,
}

func (h *Handler) apiListDirFiles(w http.ResponseWriter, r *http.Request) {
	dirName := r.URL.Query().Get("dir")
	if dirName == "" {
		writeError(w, http.StatusBadRequest, "dir is required")
		return
	}
	if !validDirNames[dirName] {
		writeError(w, http.StatusBadRequest, "invalid dir: must be one of commands, agents, skills, plugins")
		return
	}
	files, err := h.config.ListDirFiles(dirName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list files: "+err.Error())
		return
	}
	type dirFile struct {
		Name    string `json:"name"`
		RelPath string `json:"rel_path"`
	}
	result := []dirFile{}
	for _, f := range files {
		result = append(result, dirFile{Name: f.Name, RelPath: f.RelPath})
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) apiSaveDirFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10 MB limit (#17)
	var req struct {
		Dir      string `json:"dir"`
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Dir == "" || req.Filename == "" {
		writeError(w, http.StatusBadRequest, "dir and filename are required")
		return
	}

	// Special marker used by the frontend to edit agents-skills files (e.g.
	// agents-skills/skills/foo/SKILL.md). In this case Filename is already
	// the full relPath returned by ListAgentsSkills; containedPath validates it.
	var relPath string
	if req.Dir == "__agents-skill__" {
		relPath = req.Filename
	} else {
		if !validDirNames[req.Dir] {
			writeError(w, http.StatusBadRequest, "invalid dir: must be one of commands, agents, skills, plugins")
			return
		}
		// M13: validate filename — no path separators, no NUL, reasonable length.
		if strings.ContainsAny(req.Filename, "/\\\x00") || len(req.Filename) > 255 || req.Filename == "." || req.Filename == ".." {
			writeError(w, http.StatusBadRequest, "invalid filename")
			return
		}
		relPath = filepath.Join(config.DirOpenCodeConfig, req.Dir, req.Filename)
	}
	if err := h.config.WriteFile(relPath, req.Content); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save file: "+err.Error())
		return
	}
	writeNoContent(w)
}

func (h *Handler) apiDeleteDirFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := h.config.DeleteFile(relPath); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete file: "+err.Error())
		return
	}
	writeNoContent(w)
}

func (h *Handler) apiDeleteAgentsSkill(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := h.config.DeleteAgentsSkill(name); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete skill: "+err.Error())
		return
	}
	writeNoContent(w)
}

// --- WebSocket handlers (unchanged) ---

// wsUpgrader returns a WebSocket upgrader that allows same-host origins
// plus an optional extra allowed origin (e.g. the dev frontend server).
func (h *Handler) wsUpgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			// Always accept same-host origins (scheme-insensitive).
			host := r.Host
			bareOrigin := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
			if strings.EqualFold(bareOrigin, host) {
				return true
			}
			// Also accept any configured CORS origin (e.g. dev frontend).
			for _, allowed := range h.corsOrigins {
				if strings.EqualFold(origin, allowed) {
					return true
				}
			}
			// Check saved CORS origins from config (dynamic, no restart needed).
			if h.config != nil {
				if saved, err := h.config.GetCORSOrigins(); err == nil {
					for _, allowed := range saved {
						if strings.EqualFold(origin, allowed) {
							return true
						}
					}
				}
			}
			return false
		},
	}
}

func (h *Handler) handleLogsWS(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthenticatedWS(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	if inst.ContainerID == "" || h.docker == nil {
		http.Error(w, "Container not available", http.StatusBadRequest)
		return
	}

	conn, err := h.wsUpgrader().Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed for logs: %v", err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	reader, err := h.docker.ContainerLogsStream(ctx, inst.ContainerID, "200")
	if err != nil {
		log.Printf("ContainerLogsStream error for %s: %v", inst.ID, err)
		_ = conn.WriteMessage(websocket.TextMessage, []byte("Failed to stream logs"))
		return
	}
	defer reader.Close()

	conn.SetReadLimit(512) // clients send nothing on the log stream

	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			// #10: use BinaryMessage for raw log bytes (may contain ANSI codes)
			if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "logs stream ended"))
			return
		}
	}
}

func (h *Handler) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthenticatedWS(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	if inst.ContainerID == "" || h.docker == nil {
		http.Error(w, "Container not available", http.StatusBadRequest)
		return
	}

	conn, err := h.wsUpgrader().Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// #11: use a shared context so both goroutines can signal each other to stop
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	execID, err := h.docker.ExecCreate(ctx, inst.ContainerID, []string{"/bin/bash", "-l"})
	if err != nil {
		log.Printf("ExecCreate error for %s: %v", inst.ID, err)
		_ = conn.WriteMessage(websocket.TextMessage, []byte("Failed to create terminal session"))
		return
	}

	hijacked, err := h.docker.ExecAttach(ctx, execID)
	if err != nil {
		log.Printf("ExecAttach error for %s: %v", inst.ID, err)
		_ = conn.WriteMessage(websocket.TextMessage, []byte("Failed to attach terminal session"))
		return
	}
	defer hijacked.Close()

	done := make(chan struct{})

	// H4: serialize all writes to hijacked.Conn (Write + CloseWrite) with a mutex
	// so the two goroutines don't race on the underlying net.Conn.
	var connMu sync.Mutex

	// goroutine: container → WebSocket
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := hijacked.Reader.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					cancel() // #11: signal reader goroutine to stop
					return
				}
			}
			if err != nil {
				cancel() // #11
				return
			}
		}
	}()

	type resizeMsg struct {
		Type string `json:"type"`
		Cols uint   `json:"cols"`
		Rows uint   `json:"rows"`
	}

	// goroutine: WebSocket → container
	go func() {
		for {
			msgType, msg, err := conn.ReadMessage()
			if err != nil {
				connMu.Lock()
				_ = hijacked.CloseWrite()
				connMu.Unlock()
				cancel() // #11: signal writer goroutine to stop
				return
			}
			if msgType == websocket.TextMessage && len(msg) > 0 && msg[0] == '{' {
				var rm resizeMsg
				if json.Unmarshal(msg, &rm) == nil && rm.Type == "resize" {
					// Clamp dimensions to a sane range before forwarding to Docker.
					if rm.Cols < 1 {
						rm.Cols = 1
					} else if rm.Cols > 500 {
						rm.Cols = 500
					}
					if rm.Rows < 1 {
						rm.Rows = 1
					} else if rm.Rows > 500 {
						rm.Rows = 500
					}
					_ = h.docker.ExecResize(ctx, execID, rm.Rows, rm.Cols)
					continue
				}
			}
			connMu.Lock()
			_, writeErr := hijacked.Conn.Write(msg)
			connMu.Unlock()
			if writeErr != nil {
				cancel() // #11
				return
			}
		}
	}()

	<-done
}

// --- Proxy handlers (unchanged) ---

const instanceCookieName = "_cc_inst"

func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Validate the per-instance access token before proxying.
	if !h.proxy.ValidateToken(r, id) {
		// Check if token was provided as ?token= query param — if so it was
		// structurally invalid (wrong value); otherwise it is simply missing.
		if r.URL.Query().Get("token") != "" || r.Header.Get("Authorization") != "" {
			writeError(w, http.StatusUnauthorized, "invalid instance token")
		} else {
			writeError(w, http.StatusUnauthorized, "instance token required")
		}
		return
	}

	// Set the routing cookie so catch-all proxy requests know which instance
	// to route to (for SPA asset requests that don't carry the /instance/{id}/ prefix).
	http.SetCookie(w, &http.Cookie{
		Name:     instanceCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil, // #18: set Secure flag when serving over HTTPS
		SameSite: http.SameSiteLaxMode,
	})

	// If the token was provided as ?token= query param, set the per-instance
	// token cookie so subsequent requests (assets, WS) don't need it in the URL.
	if t := r.URL.Query().Get("token"); t != "" {
		proxy.SetTokenCookie(w, r, id, t)
		// C3/M1: Use 303 See Other (not 302) so the browser always follows with GET,
		// preventing any method replay.  Add Referrer-Policy so the token-bearing
		// URL is not included in the Referer header of the redirected request.
		// M2: build a clean path+query (no fragment, no scheme/host) explicitly.
		w.Header().Set("Referrer-Policy", "no-referrer")
		cleanPath := r.URL.Path
		q := r.URL.Query()
		q.Del("token")
		cleanTarget := cleanPath
		if encoded := q.Encode(); encoded != "" {
			cleanTarget += "?" + encoded
		}
		http.Redirect(w, r, cleanTarget, http.StatusSeeOther)
		return
	}

	h.proxy.ServeHTTP(w, r, id)
}

// spaSecurityHeaders sets defensive security headers on all SPA HTML responses.
func spaSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' ws: wss:; font-src 'self' data:")
}

func (h *Handler) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	// If it's an API path that wasn't matched, return 404 JSON
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// /instance/{id} without trailing slash — redirect to /instance/{id}/ so it
	// hits the registered handleProxy route.
	if strings.HasPrefix(r.URL.Path, "/instance/") && !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}

	// /login is always public — serve the SPA so it can render the login page.
	isLoginPage := r.URL.Path == "/login" || r.URL.Path == "/login/"

	// Proxied asset request (Referer or cookie based).
	// C2/H1: check once — allow if either the platform session is valid OR the
	// per-instance token cookie is present and valid.  No double-check.
	if instanceID := h.resolveInstanceID(r); instanceID != "" {
		if h.proxy.ValidateToken(r, instanceID) {
			h.proxy.ServeHTTPDirect(w, r, instanceID)
			return
		}
		if h.isAuthenticated(r) {
			// Session-authenticated admin: require the instance token cookie too,
			// which is set automatically on the first ?token= visit.
			writeError(w, http.StatusUnauthorized, "instance token required — open the instance via its token URL first")
			return
		}
		// Instance path but no auth at all.
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	// Non-instance path: require platform session (except /login).
	if !isLoginPage && !h.isAuthenticated(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Serve the embedded SPA for all other paths.
	if h.spaFS == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	data, err := fs.ReadFile(h.spaFS, "index.html")
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	spaSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (h *Handler) resolveInstanceID(r *http.Request) string {
	if id := extractInstanceIDFromReferer(r); id != "" {
		return id
	}
	// M7: validate the cookie value before using it as an instance ID.
	if c, err := r.Cookie(instanceCookieName); err == nil && instanceIDRe.MatchString(c.Value) {
		return c.Value
	}
	return ""
}

func extractInstanceIDFromReferer(r *http.Request) string {
	referer := r.Header.Get("Referer")
	if referer == "" {
		return ""
	}
	const prefix = "/instance/"
	idx := strings.Index(referer, prefix)
	if idx == -1 {
		return ""
	}
	rest := referer[idx+len(prefix):]
	slashIdx := strings.Index(rest, "/")
	if slashIdx == -1 {
		return ""
	}
	// H8: validate the extracted ID against the expected format.
	id := rest[:slashIdx]
	if !instanceIDRe.MatchString(id) {
		return ""
	}
	return id
}
