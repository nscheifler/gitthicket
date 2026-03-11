package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gitthicket/internal/auth"
	"gitthicket/internal/db"
	"gitthicket/internal/gitrepo"
	"gitthicket/internal/server"
)

func main() {
	var (
		listen           = flag.String("listen", ":8080", "listen address")
		dataDir          = flag.String("data", "./data", "data directory")
		adminKeyFlag     = flag.String("admin-key", "", "admin API key")
		maxBundleMB      = flag.Int("max-bundle-mb", 50, "maximum bundle size in MB")
		maxPushesPerHour = flag.Int("max-pushes-per-hour", 100, "per-agent push limit per hour")
		maxPostsPerHour  = flag.Int("max-posts-per-hour", 100, "per-agent post limit per hour")
	)
	flag.Parse()

	adminKey := *adminKeyFlag
	if adminKey == "" {
		adminKey = os.Getenv("GITTHICKET_ADMIN_KEY")
	}
	if adminKey == "" {
		log.Fatal("missing admin key: set --admin-key or GITTHICKET_ADMIN_KEY")
	}

	absDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("resolve data dir: %v", err)
	}
	if err := os.MkdirAll(absDataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	store, err := db.Open(filepath.Join(absDataDir, "gitthicket.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	repo, err := gitrepo.Open(filepath.Join(absDataDir, "repo.git"))
	if err != nil {
		log.Fatalf("open bare repo: %v", err)
	}

	handler := server.New(server.Config{
		MaxBundleBytes:    int64(*maxBundleMB) * 1024 * 1024,
		MaxPushesPerHour:  *maxPushesPerHour,
		MaxPostsPerHour:   *maxPostsPerHour,
		MaxCommitsPerPush: 10000,
		DiffMaxBytes:      256 * 1024,
	}, store, repo, auth.NewAuthenticator(store, adminKey))

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("gitthicket listening on %s (data=%s)", *listen, absDataDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server exited: %v", err)
	}
}
