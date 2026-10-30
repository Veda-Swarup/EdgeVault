package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vedaswarup/edgevault/internal/blob"
	"github.com/vedaswarup/edgevault/internal/gc"
	"github.com/vedaswarup/edgevault/internal/meta"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "report removals without deleting metadata or objects")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	dbURL := os.Getenv("EDGEVAULT_DB_URL")
	bucket := os.Getenv("EDGEVAULT_S3_CHUNKS_BUCKET")
	if dbURL == "" || bucket == "" {
		log.Error("EDGEVAULT_DB_URL and EDGEVAULT_S3_CHUNKS_BUCKET are required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := meta.Open(ctx, dbURL)
	if err != nil {
		log.Error("database connect", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	client, err := blob.NewS3Client(ctx, blob.S3Config{Region: env("EDGEVAULT_S3_REGION", "us-east-1"), Endpoint: os.Getenv("EDGEVAULT_S3_ENDPOINT"), ForcePathStyle: os.Getenv("EDGEVAULT_S3_FORCE_PATH_STYLE") == "true", AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")})
	if err != nil {
		log.Error("s3 client", "err", err)
		os.Exit(1)
	}
	runner := gc.Runner{Store: meta.NewStore(db), Blob: client, Bucket: bucket}
	started := time.Now()
	report, err := runner.Run(ctx, *dryRun)
	if err != nil {
		log.Error("gc failed", "err", err, "dry_run", *dryRun)
		os.Exit(1)
	}
	log.Info("gc completed", "dry_run", *dryRun, "elapsed", time.Since(started), "report", report)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
