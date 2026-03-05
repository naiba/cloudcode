package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/naiba/cloudcode/internal/config"
	"github.com/naiba/cloudcode/internal/docker"
	"github.com/naiba/cloudcode/internal/handler"
	"github.com/naiba/cloudcode/internal/proxy"
	"github.com/naiba/cloudcode/internal/store"
)

//go:embed frontend/dist
var embeddedSPA embed.FS

func main() {
	var (
		addr            = flag.String("addr", ":8080", "HTTP listen address")
		dataDir         = flag.String("data", "./data", "Data directory for SQLite database")
		imgName         = flag.String("image", "ghcr.io/naiba/cloudcode-base:latest", "Docker image name for opencode instances")
		noDocker        = flag.Bool("no-docker", false, "Skip Docker initialization (for UI preview)")
		corsOrigin      = flag.String("cors-origin", "", "Allowed CORS origin for dev (e.g. http://localhost:3000)")
		accessToken     = flag.String("access-token", "", "Required bearer token / password for accessing the platform")
		proxyCORSOrigin = flag.String("proxy-cors-origin", "", "CORS Access-Control-Allow-Origin injected into proxied instance responses (empty = disabled)")
	)
	flag.Parse()

	if *accessToken == "" {
		log.Fatal("--access-token is required. Set a strong secret token to protect the platform.")
	}

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

	spaFiles, err := fs.Sub(embeddedSPA, "frontend/dist")
	if err != nil {
		log.Fatalf("Failed to sub embedded SPA: %v", err)
	}

	rp := proxy.New(*proxyCORSOrigin)
	h := handler.New(db, dm, rp, cfgMgr, spaFiles, *accessToken, *corsOrigin)

	mux := http.NewServeMux()

	// CORS middleware for development
	var rootHandler http.Handler = mux
	if *corsOrigin != "" {
		log.Printf("CORS enabled for origin: %s", *corsOrigin)
		rootHandler = corsMiddleware(*corsOrigin, mux)
	}

	h.RegisterRoutes(mux)

	server := &http.Server{
		Addr:              *addr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,  // prevent Slowloris header attacks
		ReadTimeout:       30 * time.Second,  // limit slow-body attacks
		WriteTimeout:      120 * time.Second, // generous for streaming (logs/terminal WS handshake)
		IdleTimeout:       120 * time.Second, // reclaim idle connections
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		// #8: graceful shutdown with timeout instead of server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	log.Printf("CloudCode listening on %s", *addr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func corsMiddleware(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
