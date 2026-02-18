package main

import (
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/naiba/cloudcode/internal/config"
	"github.com/naiba/cloudcode/internal/docker"
	"github.com/naiba/cloudcode/internal/handler"
	"github.com/naiba/cloudcode/internal/proxy"
	"github.com/naiba/cloudcode/internal/store"
)

var version = "dev"

func main() {
	var (
		addr     = flag.String("addr", ":8080", "HTTP listen address")
		dataDir  = flag.String("data", "./data", "Data directory for SQLite database")
		imgName  = flag.String("image", "ghcr.io/naiba/cloudcode-base:latest", "Docker image name for opencode instances")
		noDocker = flag.Bool("no-docker", false, "Skip Docker initialization (for UI preview)")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Starting CloudCode Management Platform...")

	db, err := store.New(*dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}
	defer db.Close()

	cfgMgr, err := config.NewManager(*dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize config manager: %v", err)
	}

	var dm *docker.Manager
	if !*noDocker {
		dm, err = docker.NewManager(*imgName, cfgMgr)
		if err != nil {
			log.Fatalf("Failed to initialize Docker manager: %v", err)
		}
		defer dm.Close()

		exists, err := dm.ImageExists(nil)
		if err != nil {
			log.Printf("Warning: Could not check for base image: %v", err)
		} else if !exists {
			log.Printf("Warning: Base image %q not found. Build it first:", *imgName)
			log.Printf("  docker build -t %s -f docker/Dockerfile docker/", *imgName)
		}
	} else {
		log.Println("Docker disabled (--no-docker), container operations will fail")
	}

	rp := proxy.New()

	tmpl, err := loadTemplates()
	if err != nil {
		log.Fatalf("Failed to load templates: %v", err)
	}

	h := handler.New(db, dm, rp, cfgMgr, tmpl)

	// Setup routes
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Start server
	server := &http.Server{
		Addr:    *addr,
		Handler: mux,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		server.Close()
	}()

	log.Printf("CloudCode listening on %s", *addr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func loadTemplates() (map[string]*template.Template, error) {
	funcMap := template.FuncMap{
		"version":  func() string { return version },
		"contains": strings.Contains,
		"statusColor": func(status string) string {
			switch status {
			case "running":
				return "green"
			case "stopped", "exited":
				return "gray"
			case "error":
				return "red"
			case "created":
				return "blue"
			default:
				return "yellow"
			}
		},
		"statusBadge": func(status string) string {
			switch status {
			case "running":
				return "badge-success"
			case "stopped", "exited":
				return "badge-secondary"
			case "error":
				return "badge-danger"
			case "created":
				return "badge-info"
			default:
				return "badge-warning"
			}
		},
	}

	shared := []string{
		filepath.Join("templates", "layouts", "base.html"),
		filepath.Join("templates", "partials", "instance_row.html"),
	}

	pages, err := filepath.Glob(filepath.Join("templates", "*.html"))
	if err != nil {
		return nil, fmt.Errorf("glob pages: %w", err)
	}

	tmpls := make(map[string]*template.Template)

	for _, page := range pages {
		name := strings.TrimSuffix(filepath.Base(page), ".html")
		files := append([]string{page}, shared...)
		t, err := template.New(name).Funcs(funcMap).ParseFiles(files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		tmpls[name] = t
	}

	partials, _ := filepath.Glob(filepath.Join("templates", "partials", "*.html"))
	for _, p := range partials {
		name := strings.TrimSuffix(filepath.Base(p), ".html")
		t, err := template.New(name).Funcs(funcMap).ParseFiles(p)
		if err != nil {
			return nil, fmt.Errorf("parse partial %s: %w", p, err)
		}
		tmpls[name] = t
	}

	return tmpls, nil
}
