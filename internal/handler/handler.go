package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/naiba/cloudcode/internal/config"
	"github.com/naiba/cloudcode/internal/docker"
	"github.com/naiba/cloudcode/internal/proxy"
	"github.com/naiba/cloudcode/internal/store"
)

type Handler struct {
	store    *store.Store
	docker   *docker.Manager
	proxy    *proxy.ReverseProxy
	config   *config.Manager
	tmpls    map[string]*template.Template
	portPool *PortPool
}

// PortPool allocates ports for new instances.
type PortPool struct {
	start int
	end   int
	used  map[int]bool
}

// NewPortPool creates a port pool with the given range.
func NewPortPool(start, end int) *PortPool {
	return &PortPool{
		start: start,
		end:   end,
		used:  make(map[int]bool),
	}
}

// Allocate returns the next available port.
func (pp *PortPool) Allocate() (int, error) {
	for p := pp.start; p <= pp.end; p++ {
		if !pp.used[p] {
			pp.used[p] = true
			return p, nil
		}
	}
	return 0, fmt.Errorf("no available ports in range %d-%d", pp.start, pp.end)
}

// Release frees a port.
func (pp *PortPool) Release(port int) {
	delete(pp.used, port)
}

// MarkUsed marks a port as used.
func (pp *PortPool) MarkUsed(port int) {
	pp.used[port] = true
}

func New(s *store.Store, dm *docker.Manager, rp *proxy.ReverseProxy, cfgMgr *config.Manager, tmpls map[string]*template.Template) *Handler {
	h := &Handler{
		store:    s,
		docker:   dm,
		proxy:    rp,
		config:   cfgMgr,
		tmpls:    tmpls,
		portPool: NewPortPool(10000, 10100),
	}

	// Load existing instances and mark their ports as used
	instances, err := s.List()
	if err == nil {
		for _, inst := range instances {
			if inst.Port > 0 {
				h.portPool.MarkUsed(inst.Port)
			}
			// Register proxy for running instances
			if inst.Status == "running" && inst.Port > 0 {
				_ = rp.Register(inst.ID, inst.Port)
			}
		}
	}

	return h
}

// RegisterRoutes sets up all HTTP routes.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Static files
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	mux.HandleFunc("GET /{$}", h.handleDashboard)
	mux.HandleFunc("GET /instances/new", h.handleNewInstanceForm)
	mux.HandleFunc("GET /settings", h.handleSettings)
	mux.HandleFunc("POST /settings/env", h.handleSaveEnvVars)
	mux.HandleFunc("GET /settings/file", h.handleGetConfigFile)
	mux.HandleFunc("POST /settings/file", h.handleSaveConfigFile)
	mux.HandleFunc("GET /settings/dir-files", h.handleListDirFiles)
	mux.HandleFunc("POST /settings/dir-file", h.handleSaveDirFile)
	mux.HandleFunc("DELETE /settings/dir-file", h.handleDeleteDirFile)

	// Instance CRUD (HTMX endpoints)
	mux.HandleFunc("POST /instances", h.handleCreateInstance)
	mux.HandleFunc("GET /instances/{id}", h.handleGetInstance)
	mux.HandleFunc("DELETE /instances/{id}", h.handleDeleteInstance)

	// Instance actions
	mux.HandleFunc("POST /instances/{id}/start", h.handleStartInstance)
	mux.HandleFunc("POST /instances/{id}/stop", h.handleStopInstance)
	mux.HandleFunc("POST /instances/{id}/restart", h.handleRestartInstance)
	mux.HandleFunc("GET /instances/{id}/logs/ws", h.handleLogsWS)
	mux.HandleFunc("GET /instances/{id}/status", h.handleInstanceStatus)
	mux.HandleFunc("GET /instances/{id}/terminal", h.handleTerminalPage)
	mux.HandleFunc("GET /instances/{id}/terminal/ws", h.handleTerminalWS)

	// Reverse proxy to opencode web UI
	mux.HandleFunc("/instance/{id}/", h.handleProxy)

	// Catch-all: route non-platform requests to containers via Referer header
	mux.HandleFunc("/", h.handleCatchAll)
}

// --- Page handlers ---

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	instances, err := h.store.List()
	if err != nil {
		http.Error(w, "Failed to list instances", http.StatusInternalServerError)
		return
	}

	// Sync status with Docker
	for _, inst := range instances {
		if inst.ContainerID != "" {
			status, err := h.docker.ContainerStatus(r.Context(), inst.ContainerID)
			if err == nil && status != inst.Status {
				inst.Status = status
				_ = h.store.Update(inst)
			}
		}
	}

	data := map[string]interface{}{
		"Instances": instances,
		"Title":     "CloudCode - Dashboard",
	}
	h.render(w, "dashboard", data)
}

func (h *Handler) handleNewInstanceForm(w http.ResponseWriter, r *http.Request) {
	h.render(w, "new_instance", map[string]interface{}{
		"Title": "CloudCode - New Instance",
	})
}

// --- Instance CRUD ---

func (h *Handler) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	if existing, _ := h.store.GetByName(name); existing != nil {
		http.Error(w, "Instance name already exists", http.StatusConflict)
		return
	}

	port, err := h.portPool.Allocate()
	if err != nil {
		http.Error(w, "No available ports", http.StatusServiceUnavailable)
		return
	}

	inst := &store.Instance{
		ID:      uuid.New().String()[:8],
		Name:    name,
		Status:  "created",
		Port:    port,
		WorkDir: "/root",
		EnvVars: make(map[string]string),
	}

	if err := h.store.Create(inst); err != nil {
		h.portPool.Release(port)
		http.Error(w, "Failed to create instance", http.StatusInternalServerError)
		return
	}

	containerID, err := h.docker.CreateContainer(r.Context(), inst)
	if err != nil {
		log.Printf("Error creating container for %s: %v", inst.ID, err)
		inst.Status = "error"
		inst.ErrorMsg = err.Error()
		_ = h.store.Update(inst)
	} else {
		inst.ContainerID = containerID
		inst.Status = "running"
		_ = h.store.Update(inst)

		if err := h.proxy.Register(inst.ID, inst.Port); err != nil {
			log.Printf("Error registering proxy for %s: %v", inst.ID, err)
		}
	}

	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	// Sync status
	if inst.ContainerID != "" {
		if status, err := h.docker.ContainerStatus(r.Context(), inst.ContainerID); err == nil {
			inst.Status = status
			_ = h.store.Update(inst)
		}
	}

	data := map[string]interface{}{
		"Instance": inst,
		"Title":    fmt.Sprintf("CloudCode - %s", inst.Name),
	}
	h.render(w, "instance_detail", data)
}

func (h *Handler) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	// Remove container
	if inst.ContainerID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := h.docker.RemoveContainer(ctx, inst.ContainerID); err != nil {
			log.Printf("Error removing container for %s: %v", id, err)
		}
	}

	// Unregister proxy
	h.proxy.Unregister(id)
	h.portPool.Release(inst.Port)

	// Delete from store
	if err := h.store.Delete(id); err != nil {
		http.Error(w, "Failed to delete instance", http.StatusInternalServerError)
		return
	}

	// Check if request is from instance detail page (via Referer)
	referer := r.Header.Get("Referer")
	if referer != "" && strings.Contains(referer, "/instances/") {
		// From detail page, redirect to dashboard
		w.Header().Set("HX-Redirect", "/")
	} else {
		// From dashboard, trigger event to remove row
		w.Header().Set("HX-Trigger", "instanceDeleted")
	}
	w.WriteHeader(http.StatusOK)
}

// --- Instance actions ---

func (h *Handler) handleStartInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	if inst.ContainerID == "" {
		containerID, err := h.docker.CreateContainer(r.Context(), inst)
		if err != nil {
			inst.Status = "error"
			inst.ErrorMsg = err.Error()
			_ = h.store.Update(inst)
			respondError(w, "Failed to create container: "+err.Error())
			return
		}
		inst.ContainerID = containerID
	} else {
		if err := h.docker.StartContainer(r.Context(), inst.ContainerID); err != nil {
			inst.Status = "error"
			inst.ErrorMsg = err.Error()
			_ = h.store.Update(inst)
			respondError(w, "Failed to start container: "+err.Error())
			return
		}
	}

	inst.Status = "running"
	inst.ErrorMsg = ""
	_ = h.store.Update(inst)
	_ = h.proxy.Register(inst.ID, inst.Port)

	h.renderPartial(w, "instance_row", inst)
}

func (h *Handler) handleStopInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	if inst.ContainerID != "" {
		if err := h.docker.StopContainer(r.Context(), inst.ContainerID); err != nil {
			respondError(w, "Failed to stop container: "+err.Error())
			return
		}
	}

	inst.Status = "stopped"
	_ = h.store.Update(inst)
	h.proxy.Unregister(id)

	h.renderPartial(w, "instance_row", inst)
}

func (h *Handler) handleRestartInstance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	if inst.ContainerID != "" {
		_ = h.docker.StopContainer(r.Context(), inst.ContainerID)
		if err := h.docker.StartContainer(r.Context(), inst.ContainerID); err != nil {
			respondError(w, "Failed to restart container: "+err.Error())
			return
		}
	}

	inst.Status = "running"
	_ = h.store.Update(inst)
	_ = h.proxy.Register(inst.ID, inst.Port)

	h.renderPartial(w, "instance_row", inst)
}

func (h *Handler) handleLogsWS(w http.ResponseWriter, r *http.Request) {
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

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed for logs: %v", err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	reader, err := h.docker.ContainerLogsStream(ctx, inst.ContainerID, "200")
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("Failed to stream logs: "+err.Error()))
		return
	}
	defer reader.Close()

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
			if writeErr := conn.WriteMessage(websocket.TextMessage, buf[:n]); writeErr != nil {
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

func (h *Handler) handleInstanceStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	if inst.ContainerID != "" {
		if status, err := h.docker.ContainerStatus(r.Context(), inst.ContainerID); err == nil {
			if status != inst.Status {
				inst.Status = status
				_ = h.store.Update(inst)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": inst.Status})
}

const instanceCookieName = "_cc_inst"

func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	http.SetCookie(w, &http.Cookie{
		Name:     instanceCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	h.proxy.ServeHTTP(w, r, id)
}

func (h *Handler) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	instanceID := h.resolveInstanceID(r)
	if instanceID == "" {
		http.NotFound(w, r)
		return
	}

	h.proxy.ServeHTTPDirect(w, r, instanceID)
}

func (h *Handler) resolveInstanceID(r *http.Request) string {
	if id := extractInstanceIDFromReferer(r); id != "" {
		return id
	}
	if c, err := r.Cookie(instanceCookieName); err == nil && c.Value != "" {
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
	return rest[:slashIdx]
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	envVars, _ := h.config.GetEnvVars()
	files := h.config.EditableFiles()

	type fileData struct {
		Name    string
		RelPath string
		Hint    string
		Content string
	}

	var editableFiles []fileData
	for _, f := range files {
		content, _ := h.config.ReadFile(f.RelPath)
		editableFiles = append(editableFiles, fileData{
			Name:    f.Name,
			RelPath: f.RelPath,
			Hint:    f.Hint,
			Content: content,
		})
	}

	type dirSection struct {
		Name  string
		Hint  string
		Files []config.DirFileInfo
	}

	dirDefs := []struct {
		name string
		hint string
	}{
		{"commands", "自定义命令（.md 文件），容器内路径 ~/.config/opencode/commands/"},
		{"agents", "自定义 Agent（.md 文件），容器内路径 ~/.config/opencode/agents/"},
		{"skills", "Agent Skills（<name>/SKILL.md），容器内路径 ~/.config/opencode/skills/"},
		{"plugins", "本地 Plugin（.js/.ts 文件），容器内路径 ~/.config/opencode/plugins/"},
	}

	var dirs []dirSection
	for _, d := range dirDefs {
		dirFiles, _ := h.config.ListDirFiles(d.name)
		dirs = append(dirs, dirSection{
			Name:  d.name,
			Hint:  d.hint,
			Files: dirFiles,
		})
	}

	data := map[string]interface{}{
		"Title":     "CloudCode - Settings",
		"EnvVars":   envVars,
		"Files":     editableFiles,
		"Dirs":      dirs,
		"ConfigDir": h.config.RootDir(),
	}
	h.render(w, "settings", data)
}

func (h *Handler) handleSaveEnvVars(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	env := make(map[string]string)
	keys := r.Form["env_key"]
	values := r.Form["env_value"]
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		v := ""
		if i < len(values) {
			v = strings.TrimSpace(values[i])
		}
		env[k] = v
	}

	if err := h.config.SetEnvVars(env); err != nil {
		respondError(w, "Failed to save environment variables: "+err.Error())
		return
	}

	w.Header().Set("HX-Redirect", "/settings")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleGetConfigFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	content, err := h.config.ReadFile(relPath)
	if err != nil {
		http.Error(w, "Failed to read file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, content)
}

func (h *Handler) handleSaveConfigFile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	relPath := r.FormValue("path")
	content := r.FormValue("content")
	if relPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	if err := h.config.WriteFile(relPath, content); err != nil {
		respondError(w, "Failed to save file: "+err.Error())
		return
	}

	w.Header().Set("HX-Redirect", "/settings")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleListDirFiles(w http.ResponseWriter, r *http.Request) {
	dirName := r.URL.Query().Get("dir")
	if dirName == "" {
		http.Error(w, "dir is required", http.StatusBadRequest)
		return
	}

	files, err := h.config.ListDirFiles(dirName)
	if err != nil {
		http.Error(w, "Failed to list files: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func (h *Handler) handleSaveDirFile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	dir := r.FormValue("dir")
	filename := r.FormValue("filename")
	content := r.FormValue("content")
	if dir == "" || filename == "" {
		http.Error(w, "dir and filename are required", http.StatusBadRequest)
		return
	}

	relPath := filepath.Join(config.DirOpenCodeConfig, dir, filename)
	if err := h.config.WriteFile(relPath, content); err != nil {
		respondError(w, "Failed to save file: "+err.Error())
		return
	}

	w.Header().Set("HX-Redirect", "/settings")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleDeleteDirFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	if err := h.config.DeleteFile(relPath); err != nil {
		respondError(w, "Failed to delete file: "+err.Error())
		return
	}

	w.Header().Set("HX-Redirect", "/settings")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) render(w http.ResponseWriter, name string, data interface{}) {
	t, ok := h.tmpls[name]
	if !ok {
		log.Printf("Template not found: %s", name)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("Template render error (%s): %v", name, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func (h *Handler) renderPartial(w http.ResponseWriter, name string, data interface{}) {
	t, ok := h.tmpls[name]
	if !ok {
		log.Printf("Partial template not found: %s", name)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("Partial render error (%s): %v", name, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (h *Handler) handleTerminalPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := h.store.Get(id)
	if err != nil {
		http.Error(w, "Instance not found", http.StatusNotFound)
		return
	}

	data := map[string]interface{}{
		"Instance": inst,
		"Title":    fmt.Sprintf("CloudCode - %s Terminal", inst.Name),
	}
	h.render(w, "terminal", data)
}

func (h *Handler) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
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

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	ctx := r.Context()

	execID, err := h.docker.ExecCreate(ctx, inst.ContainerID, []string{"/bin/bash", "-l"})
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("Failed to create exec: "+err.Error()))
		return
	}

	hijacked, err := h.docker.ExecAttach(ctx, execID)
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("Failed to attach exec: "+err.Error()))
		return
	}
	defer hijacked.Close()

	done := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := hijacked.Reader.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	type resizeMsg struct {
		Type string `json:"type"`
		Cols uint   `json:"cols"`
		Rows uint   `json:"rows"`
	}

	go func() {
		for {
			msgType, msg, err := conn.ReadMessage()
			if err != nil {
				_ = hijacked.CloseWrite()
				return
			}

			if msgType == websocket.TextMessage && len(msg) > 0 && msg[0] == '{' {
				var rm resizeMsg
				if json.Unmarshal(msg, &rm) == nil && rm.Type == "resize" {
					_ = h.docker.ExecResize(ctx, execID, rm.Rows, rm.Cols)
					continue
				}
			}

			if _, err := hijacked.Conn.Write(msg); err != nil {
				return
			}
		}
	}()

	<-done
}

func respondError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<div class="alert alert-error">%s</div>`, template.HTMLEscapeString(msg))
}
