// cmd/edge/main.go — EdgeVault edge node entry point.
//
// Startup sequence:
//  1. Load config from environment.
//  2. Run database migrations.
//  3. Connect the DB pool.
//  4. Construct dependencies (chunker, blob client, store, handler).
//  5. Start the HTTP server.
//  6. Block until SIGINT/SIGTERM, then gracefully drain.
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/vedaswarup/edgevault/internal/api"
	"github.com/vedaswarup/edgevault/internal/blob"
	"github.com/vedaswarup/edgevault/internal/cache"
	"github.com/vedaswarup/edgevault/internal/chunker"
	"github.com/vedaswarup/edgevault/internal/config"
	"github.com/vedaswarup/edgevault/internal/crypto"
	"github.com/vedaswarup/edgevault/internal/journal"
	"github.com/vedaswarup/edgevault/internal/kms"
	"github.com/vedaswarup/edgevault/internal/meta"
	"github.com/vedaswarup/edgevault/internal/metrics"
	"github.com/vedaswarup/edgevault/internal/uploader"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	// --- Migrations ---
	runMigrations(log, cfg)

	// --- DB pool ---
	ctx := context.Background()
	db, err := meta.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	// --- Blob (S3 / MinIO) ---
	s3Cfg := blob.S3Config{
		Region:         cfg.S3Region,
		Endpoint:       cfg.S3Endpoint,
		ForcePathStyle: cfg.S3ForcePathStyle,
		// Static credentials for MinIO.  In production the pod's IRSA service
		// account provides credentials automatically via the SDK's default chain.
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	blobClient, err := blob.NewS3Client(ctx, s3Cfg)
	if err != nil {
		log.Error("blob init", "err", err)
		os.Exit(1)
	}

	var keyManager kms.KMS
	if cfg.KMSFake {
		keyManager = kms.FakeKMS{}
	} else {
		keyManager, err = kms.NewAWS(ctx, cfg.S3Region)
		if err != nil {
			log.Error("kms init", "err", err)
			os.Exit(1)
		}
	}

	// --- HMAC key ---
	hmacKey, err := crypto.ParseHMACKey(cfg.ChunkHMACKey)
	if err != nil {
		log.Error("hmac key", "err", err)
		os.Exit(1)
	}

	// --- Wire up ---
	store := meta.NewStore(db)
	volumes, err := store.ListVolumes(ctx)
	if err != nil {
		log.Error("list volumes for key restore", "err", err)
		os.Exit(1)
	}
	volumeKeys := make(map[int64][]byte)
	for _, v := range volumes {
		if v.KeyVersion >= 2 {
			key, err := keyManager.Decrypt(ctx, v.WrappedDataKey)
			if err != nil {
				log.Error("unwrap volume key", "volume", v.Name, "err", err)
				os.Exit(1)
			}
			if len(key) != 32 {
				log.Error("invalid volume key length", "volume", v.Name, "length", len(key))
				os.Exit(1)
			}
			volumeKeys[v.ID] = key
		}
	}
	chkr := chunker.NewFixed(chunker.DefaultChunkSize)
	diskCache, err := cache.Open(cfg.CachePath, cfg.CacheMaxBytes)
	if err != nil {
		log.Error("cache init", "err", err)
		os.Exit(1)
	}
	j, replay, err := journal.Open(cfg.CachePath + "/journal.jsonl")
	if err != nil {
		log.Error("journal init", "err", err)
		os.Exit(1)
	}
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	metricSet := &metrics.Metrics{}
	up := uploader.New(workerCtx, blobClient, diskCache, j, replay, metricSet, 2)
	handler := api.NewHandlerWithFullRuntime(store, blobClient, chkr, hmacKey, cfg.S3ChunksBucket, log, diskCache, j, up, metricSet, keyManager, cfg.KMSKeyID, volumeKeys)
	handler.SetSnapshotBucket(cfg.S3SnapshotsBucket)
	handler.SetAdminToken(cfg.AdminToken)
	srv := api.NewServer(handler, cfg.ListenAddr, log)
	if cfg.TLSCertFile != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	// --- Start server ---
	errCh := make(chan error, 1)
	go func() {
		log.Info("edge node listening", "addr", cfg.ListenAddr)
		var err error
		if cfg.TLSCertFile != "" {
			err = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// --- Graceful shutdown ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-quit:
		log.Info("shutting down", "signal", sig.String())
	case err := <-errCh:
		log.Error("server error", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := api.Shutdown(shutdownCtx, srv); err != nil {
		log.Error("shutdown", "err", err)
		os.Exit(1)
	}
	stopWorkers()
	up.Close()
	if err := j.Close(); err != nil {
		log.Error("journal close", "err", err)
	}
	log.Info("edge node stopped")
}

func runMigrations(log *slog.Logger, cfg *config.Config) {
	// golang-migrate pgx5 driver expects "pgx5://" scheme.
	dsn := cfg.DatabaseURL
	if strings.HasPrefix(dsn, "postgres://") {
		dsn = "pgx5://" + dsn[len("postgres://"):]
	}
	// Add sslmode=disable for local dev (no TLS on docker container).
	if !strings.Contains(dsn, "sslmode") {
		if strings.Contains(dsn, "?") {
			dsn += "&sslmode=disable"
		} else {
			dsn += "?sslmode=disable"
		}
	}

	m, err := migrate.New("file://"+cfg.MigrationsPath, dsn)
	if err != nil {
		log.Error("migrations init", "err", err)
		os.Exit(1)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		log.Error("migrations up", "err", err)
		os.Exit(1)
	}

	src, db := m.Close()
	if src != nil {
		log.Warn("close migration source", "err", src)
	}
	if db != nil {
		log.Warn("close migration db", "err", db)
	}
	log.Info("migrations applied")

	// Small delay to let the migration connection release cleanly before the
	// pool connects.  This avoids a rare "too many connections" race on startup.
	time.Sleep(200 * time.Millisecond)
}
