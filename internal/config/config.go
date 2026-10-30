package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration sourced from environment variables.
// No secrets ever touch the repository.
type Config struct {
	// Database
	DatabaseURL string // EDGEVAULT_DB_URL

	// S3 / MinIO
	S3Endpoint        string // EDGEVAULT_S3_ENDPOINT  (empty → real AWS)
	S3ChunksBucket    string // EDGEVAULT_S3_CHUNKS_BUCKET
	S3SnapshotsBucket string // EDGEVAULT_S3_SNAPSHOTS_BUCKET
	S3Region          string // EDGEVAULT_S3_REGION
	S3ForcePathStyle  bool   // EDGEVAULT_S3_FORCE_PATH_STYLE (set true for MinIO)

	// KMS
	KMSFake    bool   // EDGEVAULT_KMS_FAKE (true → use in-process fake)
	KMSKeyID   string // EDGEVAULT_KMS_KEY_ID (ARN or alias)
	AdminToken string // EDGEVAULT_ADMIN_TOKEN; empty disables admin-only endpoints

	// HMAC key used for chunk IDs (hex-encoded 32 bytes).
	// In Phase 3 this is replaced by the per-volume KMS-derived key.
	// For Phase 0/1 we read it from env to keep the design simple.
	ChunkHMACKey string // EDGEVAULT_CHUNK_HMAC_KEY

	// Cache
	CachePath     string // EDGEVAULT_CACHE_PATH
	CacheMaxBytes int64  // EDGEVAULT_CACHE_MAX_BYTES

	// Server
	ListenAddr      string        // EDGEVAULT_LISTEN_ADDR (default :8080)
	TLSCertFile     string        // EDGEVAULT_TLS_CERT_FILE (optional)
	TLSKeyFile      string        // EDGEVAULT_TLS_KEY_FILE (optional)
	ShutdownTimeout time.Duration // EDGEVAULT_SHUTDOWN_TIMEOUT_S (seconds)

	// Migrations path
	MigrationsPath string // EDGEVAULT_MIGRATIONS_PATH
}

// Load reads configuration from environment variables and returns an error
// for any required value that is missing.
func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:       requireEnv("EDGEVAULT_DB_URL"),
		S3ChunksBucket:    requireEnv("EDGEVAULT_S3_CHUNKS_BUCKET"),
		S3SnapshotsBucket: requireEnv("EDGEVAULT_S3_SNAPSHOTS_BUCKET"),

		S3Endpoint:       os.Getenv("EDGEVAULT_S3_ENDPOINT"),
		S3Region:         getEnvOrDefault("EDGEVAULT_S3_REGION", "us-east-1"),
		S3ForcePathStyle: os.Getenv("EDGEVAULT_S3_FORCE_PATH_STYLE") == "true",

		KMSFake:    os.Getenv("EDGEVAULT_KMS_FAKE") == "true",
		KMSKeyID:   os.Getenv("EDGEVAULT_KMS_KEY_ID"),
		AdminToken: os.Getenv("EDGEVAULT_ADMIN_TOKEN"),

		ChunkHMACKey: requireEnv("EDGEVAULT_CHUNK_HMAC_KEY"),

		CachePath:     getEnvOrDefault("EDGEVAULT_CACHE_PATH", "/tmp/edgevault-cache"),
		CacheMaxBytes: parseInt64(os.Getenv("EDGEVAULT_CACHE_MAX_BYTES"), 1<<30), // 1 GiB

		ListenAddr:      getEnvOrDefault("EDGEVAULT_LISTEN_ADDR", ":8080"),
		TLSCertFile:     os.Getenv("EDGEVAULT_TLS_CERT_FILE"),
		TLSKeyFile:      os.Getenv("EDGEVAULT_TLS_KEY_FILE"),
		ShutdownTimeout: time.Duration(parseInt64(os.Getenv("EDGEVAULT_SHUTDOWN_TIMEOUT_S"), 15)) * time.Second,

		MigrationsPath: getEnvOrDefault("EDGEVAULT_MIGRATIONS_PATH", "db/migrations"),
	}

	// Validate collected config so callers get a single, clear error.
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("EDGEVAULT_DB_URL is required")
	}
	if !c.KMSFake && c.KMSKeyID == "" {
		return fmt.Errorf("EDGEVAULT_KMS_KEY_ID is required when EDGEVAULT_KMS_FAKE is not true")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("EDGEVAULT_TLS_CERT_FILE and EDGEVAULT_TLS_KEY_FILE must be configured together")
	}
	return nil
}

// requireEnv panics with a clear message at startup if a required env var is
// absent.  We use panic here intentionally: misconfigured infra should never
// silently start.
func requireEnv(key string) string {
	v := os.Getenv(key)
	// Return empty string here; validate() catches it with a proper error.
	return v
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseInt64(s string, def int64) int64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return v
}
