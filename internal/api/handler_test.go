package api_test

// Phase 1 integration tests.
//
// These tests use:
//   - blob.MemBlob  (in-memory; no real S3 needed)
//   - meta.Store against a real PostgreSQL (set EDGEVAULT_TEST_DB_URL)
//   - The full HTTP handler stack via httptest.NewServer
//
// Run with:
//
//	EDGEVAULT_TEST_DB_URL="postgres://edgevault:edgevault@localhost:5433/edgevault_test" \
//	EDGEVAULT_CHUNK_HMAC_KEY="$(openssl rand -hex 32)"                                  \
//	go test ./internal/api/ -v -count=1 -run TestPhase1
//
// If EDGEVAULT_TEST_DB_URL is not set, the tests are skipped (not failed)
// so the CI pipeline can run unit tests without a database.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vedaswarup/edgevault/internal/api"
	"github.com/vedaswarup/edgevault/internal/blob"
	"github.com/vedaswarup/edgevault/internal/chunker"
	"github.com/vedaswarup/edgevault/internal/crypto"
	gcengine "github.com/vedaswarup/edgevault/internal/gc"
	"github.com/vedaswarup/edgevault/internal/kms"
	"github.com/vedaswarup/edgevault/internal/meta"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB returns a pool connected to the test database and applies migrations.
// The test database is cleaned up (tables dropped) after the test.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("EDGEVAULT_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("EDGEVAULT_TEST_DB_URL not set; skipping integration test")
	}

	// golang-migrate pgx5 driver requires:
	//   - scheme prefix "pgx5://" instead of "postgres://"
	//   - sslmode=disable for plain local containers
	migrateURL := "pgx5://" + dbURL[len("postgres://"):]
	if !strings.Contains(migrateURL, "sslmode") {
		if strings.Contains(migrateURL, "?") {
			migrateURL += "&sslmode=disable"
		} else {
			migrateURL += "?sslmode=disable"
		}
	}

	m, err := migrate.New("file://../../db/migrations", migrateURL)
	if err != nil {
		t.Fatalf("migrate new: %v", err)
	}
	// Reset to a clean state before each test.
	_ = m.Down()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	m.Close()

	// pgxpool also needs sslmode=disable for the local container.
	poolURL := dbURL
	if !strings.Contains(poolURL, "sslmode") {
		if strings.Contains(poolURL, "?") {
			poolURL += "&sslmode=disable"
		} else {
			poolURL += "?sslmode=disable"
		}
	}

	pool, err := pgxpool.New(context.Background(), poolURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// hmacKey returns a fixed test key (32 bytes of 0xAA).
func testHMACKey(t *testing.T) []byte {
	t.Helper()
	hex := os.Getenv("EDGEVAULT_CHUNK_HMAC_KEY")
	if hex == "" {
		// Use a fixed key for reproducible test chunk IDs.
		hex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	key, err := crypto.ParseHMACKey(hex)
	if err != nil {
		t.Fatalf("hmac key: %v", err)
	}
	return key
}

// newTestServer wires up a full handler stack against the given DB and blob.
func newTestServer(t *testing.T, pool *pgxpool.Pool, bl blob.Blob) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := meta.NewStore(pool)
	chkr := chunker.NewFixed(100) // small chunks for fast tests
	h := api.NewHandlerWithKMS(store, bl, chkr, testHMACKey(t), "test-bucket", log, kms.FakeKMS{}, "test-kms", nil)
	h.SetSnapshotBucket("test-protected")
	h.SetAdminToken("integration-admin-token")
	srv := api.NewServer(h, ":0", log)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

// createVolume is a test helper that POST /v1/volumes and returns the volume name.
func createVolume(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()
	body := `{"name":"` + name + `"}`
	resp, err := ts.Client().Post(ts.URL+"/v1/volumes", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create volume got %d: %s", resp.StatusCode, b)
	}
}

// TestPhase1_NewVersion uploads v1, changes one chunk, uploads v2, and checks
// that exactly one new S3 object was created for the changed chunk.
func TestPhase1_NewVersion(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)

	createVolume(t, ts, "vol1")

	// v1: 200 bytes of 'A'
	v1Data := bytes.Repeat([]byte("A"), 200)
	resp := put(t, ts, "vol1", "docs/readme.txt", v1Data, "")
	if resp["status"] != "created" {
		t.Fatalf("v1 status = %q, want created", resp["status"])
	}
	s3CountAfterV1 := mem.Len()
	t.Logf("S3 objects after v1: %d", s3CountAfterV1)

	// v2: second chunk changed to 'B'; first 100 bytes (chunk 0) stay 'A'
	v2Data := append(bytes.Repeat([]byte("A"), 100), bytes.Repeat([]byte("B"), 100)...)
	resp = put(t, ts, "vol1", "docs/readme.txt", v2Data, "1")
	if resp["status"] != "created" {
		t.Fatalf("v2 status = %q, want created", resp["status"])
	}
	s3CountAfterV2 := mem.Len()
	t.Logf("S3 objects after v2: %d", s3CountAfterV2)

	// Exactly one new S3 object (the changed second chunk).
	newObjects := s3CountAfterV2 - s3CountAfterV1
	if newObjects != 1 {
		t.Fatalf("expected exactly 1 new S3 object for v2, got %d", newObjects)
	}
}

// TestPhase1_IdenticalReupload uploads the same content twice and expects HTTP 200
// "unchanged" on the second upload, with no new S3 object.
func TestPhase1_IdenticalReupload(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)

	createVolume(t, ts, "vol2")

	data := bytes.Repeat([]byte("hello world"), 10)

	put(t, ts, "vol2", "file.txt", data, "")
	s3After1 := mem.Len()

	resp := put(t, ts, "vol2", "file.txt", data, "1")
	if resp["status"] != "unchanged" {
		t.Fatalf("second upload status = %q, want unchanged", resp["status"])
	}
	if mem.Len() != s3After1 {
		t.Fatalf("S3 count changed on identical re-upload: was %d, now %d", s3After1, mem.Len())
	}
}

// TestPhase1_ReadVersions uploads v1 and v2, then reads both back and checks
// byte-for-byte fidelity.
func TestPhase1_ReadVersions(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)

	createVolume(t, ts, "vol3")

	v1Data := bytes.Repeat([]byte("version-one-"), 50) // 600 bytes, 6 chunks of 100
	put(t, ts, "vol3", "data.bin", v1Data, "")

	v2Data := bytes.Repeat([]byte("version-two-"), 50)
	put(t, ts, "vol3", "data.bin", v2Data, "1")

	// Read v1
	gotV1 := getFile(t, ts, "vol3", "data.bin", "1")
	if !bytes.Equal(gotV1, v1Data) {
		t.Fatalf("v1 round-trip mismatch: got %d bytes, want %d", len(gotV1), len(v1Data))
	}

	// Read v2 (current)
	gotV2 := getFile(t, ts, "vol3", "data.bin", "")
	if !bytes.Equal(gotV2, v2Data) {
		t.Fatalf("v2 round-trip mismatch: got %d bytes, want %d", len(gotV2), len(v2Data))
	}
}

// TestPhase1_DeleteThenRead deletes a file and verifies the read returns 410.
func TestPhase1_DeleteThenRead(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)

	createVolume(t, ts, "vol4")

	put(t, ts, "vol4", "to-delete.txt", []byte("bye"), "")

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/volumes/vol4/files/to-delete.txt", nil)
	req.Header.Set("X-Edge-Node", "test-edge")
	req.Header.Set("X-Edge-Lease-Token", fmt.Sprint(acquireLease(t, ts, "vol4", "to-delete.txt", "test-edge")))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete got %d", resp.StatusCode)
	}

	// Now GET should return 410 Gone.
	getResp, err := ts.Client().Get(ts.URL + "/v1/volumes/vol4/files/to-delete.txt")
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusGone {
		t.Fatalf("expected 410 after delete, got %d", getResp.StatusCode)
	}
}

type tamperBlob struct {
	*blob.MemBlob
	tamperNext bool
}

func (b *tamperBlob) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	r, err := b.MemBlob.Get(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if b.tamperNext {
		b.tamperNext = false
		if len(data) > 0 {
			data[len(data)-1] ^= 0x80
		}
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestPhase3_TamperedCiphertextFailsRead(t *testing.T) {
	pool := testDB(t)
	bl := &tamperBlob{MemBlob: blob.NewMemBlob()}
	ts := newTestServer(t, pool, bl)
	createVolume(t, ts, "encrypted")
	put(t, ts, "encrypted", "file.txt", []byte("plaintext that must not appear in S3"), "")
	bl.tamperNext = true
	resp, err := ts.Client().Get(ts.URL + "/v1/volumes/encrypted/files/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("tampered read status=%d body=%s", resp.StatusCode, body)
	}
}

func TestPhase3_SnapshotStateAndNonDestructiveRestore(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)
	createVolume(t, ts, "restore")
	first := []byte("clean version")
	put(t, ts, "restore", "docs/a.txt", first, "")
	snapResp, err := ts.Client().Post(ts.URL+"/v1/volumes/restore/snapshots", "application/json", strings.NewReader(`{"reason":"before-change"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer snapResp.Body.Close()
	var snap map[string]any
	if err = json.NewDecoder(snapResp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snapResp.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot status=%d payload=%v", snapResp.StatusCode, snap)
	}
	put(t, ts, "restore", "docs/a.txt", []byte("changed version"), "1")
	put(t, ts, "restore", "docs/new.txt", []byte("created later"), "")
	stateResp, err := ts.Client().Get(ts.URL + "/v1/volumes/restore/state?as_of_seq=1")
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	_ = json.NewDecoder(stateResp.Body).Decode(&state)
	stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusOK {
		t.Fatalf("state status=%d: %v", stateResp.StatusCode, state)
	}
	files := state["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["path"] != "docs/a.txt" {
		t.Fatalf("state at seq 1 = %v", files)
	}
	restoreBody := fmt.Sprintf(`{"snapshot_id":%d}`, int64(snap["id"].(float64)))
	restoreResp, err := ts.Client().Post(ts.URL+"/v1/volumes/restore/restore", "application/json", strings.NewReader(restoreBody))
	if err != nil {
		t.Fatal(err)
	}
	defer restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(restoreResp.Body)
		t.Fatalf("restore status=%d body=%s", restoreResp.StatusCode, b)
	}
	if got := getFile(t, ts, "restore", "docs/a.txt", ""); !bytes.Equal(got, first) {
		t.Fatalf("restored bytes %q, want %q", got, first)
	}
	missing, err := ts.Client().Get(ts.URL + "/v1/volumes/restore/files/docs/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusGone {
		t.Fatalf("post-snapshot file status=%d, want 410", missing.StatusCode)
	}
}

func TestPhase3_ModelBasedWritesDeletesAndRestores(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)
	createVolume(t, ts, "model-based")
	store := meta.NewStore(pool)
	volume, err := store.GetVolumeByName(context.Background(), "model-based")
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		version int64
		data    []byte
	}
	clone := func(in map[string]entry) map[string]entry {
		out := make(map[string]entry, len(in))
		for name, value := range in {
			out[name] = entry{version: value.version, data: append([]byte(nil), value.data...)}
		}
		return out
	}
	current := map[string]entry{}
	history := []map[string]entry{clone(current)} // state at commit sequence zero
	versions := map[string]int64{}
	rng := rand.New(rand.NewSource(0xED6E))
	paths := []string{"a.txt", "b.txt", "nested/c.txt", "nested/d.txt"}
	seq := int64(0)
	record := func() {
		head, e := store.GetVolumeByName(context.Background(), "model-based")
		if e != nil {
			t.Fatal(e)
		}
		if head.HeadSeq != seq+1 {
			t.Fatalf("model sequence=%d but database head=%d", seq, head.HeadSeq)
		}
		seq = head.HeadSeq
		history = append(history, clone(current))
	}
	for step := 0; step < 48; step++ {
		path := paths[rng.Intn(len(paths))]
		choice := rng.Intn(10)
		if step > 5 && step%9 == 8 {
			// Restore an earlier modeled state. Handler restore commits paths in
			// sorted order, making every intermediate commit sequence testable.
			targetSeq := rng.Intn(len(history) - 1)
			target := clone(history[targetSeq])
			before := seq
			var removed []string
			for p := range current {
				if _, ok := target[p]; !ok {
					removed = append(removed, p)
				}
			}
			sort.Strings(removed)
			for _, p := range removed {
				delete(current, p)
				versions[p]++
				seq++
				history = append(history, clone(current))
			}
			var restored []string
			for p := range target {
				restored = append(restored, p)
			}
			sort.Strings(restored)
			for _, p := range restored {
				old, exists := current[p]
				want := target[p]
				if exists && bytes.Equal(old.data, want.data) {
					continue
				}
				versions[p]++
				current[p] = entry{version: versions[p], data: want.data}
				seq++
				history = append(history, clone(current))
			}
			body := fmt.Sprintf(`{"as_of_seq":%d}`, targetSeq)
			resp, e := ts.Client().Post(ts.URL+"/v1/volumes/model-based/restore", "application/json", strings.NewReader(body))
			if e != nil {
				t.Fatal(e)
			}
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("restore target %d status=%d: %s", targetSeq, resp.StatusCode, b)
			}
			resp.Body.Close()
			if seq-before != int64(len(history)-1)-before {
				t.Fatal("model restore sequence accounting failed")
			}
			actual, e := store.GetVolumeByName(context.Background(), "model-based")
			if e != nil || actual.HeadSeq != seq {
				t.Fatalf("restore head=%d model=%d err=%v", actual.HeadSeq, seq, e)
			}
			continue
		}
		if choice < 3 {
			if _, exists := current[path]; !exists {
				continue
			}
			token := acquireLease(t, ts, "model-based", path, "test-edge")
			req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/volumes/model-based/files/"+path, nil)
			req.Header.Set("X-Edge-Node", "test-edge")
			req.Header.Set("X-Edge-Lease-Token", fmt.Sprint(token))
			resp, e := ts.Client().Do(req)
			if e != nil {
				t.Fatal(e)
			}
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("delete %s status=%d: %s", path, resp.StatusCode, b)
			}
			resp.Body.Close()
			delete(current, path)
			versions[path]++
			record()
			continue
		}
		var data []byte
		if choice == 9 {
			if previous, exists := current[path]; exists {
				data = previous.data // explicit unchanged upload
			}
		}
		if data == nil {
			data = []byte(fmt.Sprintf("step=%02d path=%s payload=%016x", step, path, rng.Uint64()))
		}
		match := ""
		if versions[path] > 0 {
			match = fmt.Sprint(versions[path])
		}
		result := put(t, ts, "model-based", path, data, match)
		if result["status"] == "unchanged" {
			continue
		}
		versions[path] = int64(result["version"].(float64))
		current[path] = entry{version: versions[path], data: append([]byte(nil), data...)}
		record()
	}
	if int64(len(history)-1) != seq {
		t.Fatalf("history entries=%d do not cover commit sequences 0..%d", len(history), seq)
	}
	for asOf, expected := range history {
		resp, err := ts.Client().Get(fmt.Sprintf("%s/v1/volumes/model-based/state?as_of_seq=%d", ts.URL, asOf))
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			Files []struct {
				Path    string `json:"path"`
				Version int64  `json:"version"`
			} `json:"files"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&state); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(state.Files) != len(expected) {
			t.Fatalf("as_of_seq=%d status=%d files=%v want=%v", asOf, resp.StatusCode, state.Files, expected)
		}
		for _, actual := range state.Files {
			want, ok := expected[actual.Path]
			if !ok || actual.Version != want.version {
				t.Fatalf("as_of_seq=%d %s=%d, expected %v", asOf, actual.Path, actual.Version, want)
			}
			got := getFile(t, ts, "model-based", actual.Path, fmt.Sprint(actual.Version))
			if !bytes.Equal(got, want.data) {
				t.Fatalf("as_of_seq=%d %s version %d differs from model", asOf, actual.Path, actual.Version)
			}
		}
	}
	_ = volume // volume existence is checked above; retain ID for readable test setup.
}

func TestConcurrentWritersHaveOneWinner(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)
	createVolume(t, ts, "writers")
	put(t, ts, "writers", "shared.txt", []byte("base"), "")
	const writers = 12
	leaseToken := acquireLease(t, ts, "writers", "shared.txt", "test-edge")
	start := make(chan struct{})
	type result struct {
		id           int
		status       int
		body, data   []byte
		conflictPath string
	}
	results := make(chan result, writers)
	for i := 0; i < writers; i++ {
		go func(id int) {
			data := []byte(fmt.Sprintf("writer-%02d-content", id))
			<-start
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/volumes/writers/files/shared.txt", bytes.NewReader(data))
			req.Header.Set("If-Match", "1")
			req.Header.Set("X-Edge-Node", "test-edge")
			req.Header.Set("X-Edge-Lease-Token", fmt.Sprint(leaseToken))
			resp, err := ts.Client().Do(req)
			if err != nil {
				results <- result{id: id, body: []byte(err.Error()), data: data}
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			results <- result{id: id, status: resp.StatusCode, body: body, data: data, conflictPath: resp.Header.Get("X-EdgeVault-Conflict-Path")}
		}(i)
	}
	close(start)
	successes, conflicts := 0, 0
	var winningData []byte
	var conflictCopies int
	for i := 0; i < writers; i++ {
		r := <-results
		switch r.status {
		case http.StatusCreated:
			successes++
			winningData = r.data
		case http.StatusConflict:
			conflicts++
			if r.conflictPath != "" {
				conflictCopies++
				if got := getFile(t, ts, "writers", r.conflictPath, ""); !bytes.Equal(got, r.data) {
					t.Fatalf("conflict copy %s did not preserve rejected writer %d bytes", r.conflictPath, r.id)
				}
			}
		default:
			t.Fatalf("writer %d got HTTP %d: %s", r.id, r.status, r.body)
		}
	}
	if successes != 1 || conflicts != writers-1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	if conflictCopies != writers-1 {
		t.Fatalf("conflict copies=%d, want %d", conflictCopies, writers-1)
	}
	if got := getFile(t, ts, "writers", "shared.txt", ""); !bytes.Equal(got, winningData) {
		t.Fatalf("current content %q != sole winner %q", got, winningData)
	}
	versionsResp, err := ts.Client().Get(ts.URL + "/v1/volumes/writers/files/shared.txt/versions")
	if err != nil {
		t.Fatal(err)
	}
	defer versionsResp.Body.Close()
	var versions map[string][]any
	if err = json.NewDecoder(versionsResp.Body).Decode(&versions); err != nil {
		t.Fatal(err)
	}
	if got := len(versions["versions"]); got != 2 {
		t.Fatalf("version count=%d, want base plus one committed writer", got)
	}
}

func TestTwoEdgeHandlersShareVolumeKeyAndFenceConcurrentWriters(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	edgeA := newTestServer(t, pool, mem)
	edgeB := newTestServer(t, pool, mem)
	createVolume(t, edgeA, "two-edges")
	base := []byte("shared encrypted volume created after both edges started")
	put(t, edgeA, "two-edges", "shared.txt", base, "")
	if got := getFile(t, edgeB, "two-edges", "shared.txt", ""); !bytes.Equal(got, base) {
		t.Fatalf("second edge could not unwrap volume key: %q", got)
	}
	leaseToken := acquireLease(t, edgeA, "two-edges", "shared.txt", "test-edge")
	const writers = 10
	start := make(chan struct{})
	type result struct {
		status       int
		data         []byte
		conflictPath string
		body         []byte
	}
	results := make(chan result, writers)
	for i := 0; i < writers; i++ {
		go func(id int) {
			data := []byte(fmt.Sprintf("edge-writer-%02d", id))
			<-start
			ts := edgeA
			if id%2 == 1 {
				ts = edgeB
			}
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/volumes/two-edges/files/shared.txt", bytes.NewReader(data))
			req.Header.Set("If-Match", "1")
			req.Header.Set("X-Edge-Node", "test-edge")
			req.Header.Set("X-Edge-Lease-Token", fmt.Sprint(leaseToken))
			resp, err := ts.Client().Do(req)
			if err != nil {
				results <- result{body: []byte(err.Error()), data: data}
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			results <- result{status: resp.StatusCode, data: data, conflictPath: resp.Header.Get("X-EdgeVault-Conflict-Path"), body: body}
		}(i)
	}
	close(start)
	successes, conflicts := 0, 0
	var winner []byte
	for i := 0; i < writers; i++ {
		got := <-results
		switch got.status {
		case http.StatusCreated:
			successes++
			winner = got.data
		case http.StatusConflict:
			conflicts++
			if got.conflictPath == "" {
				t.Fatalf("conflict response omitted copy path: %s", got.body)
			}
			if data := getFile(t, edgeA, "two-edges", got.conflictPath, ""); !bytes.Equal(data, got.data) {
				t.Fatalf("conflict copy %s lost writer bytes", got.conflictPath)
			}
		default:
			t.Fatalf("edge write status=%d body=%s", got.status, got.body)
		}
	}
	if successes != 1 || conflicts != writers-1 {
		t.Fatalf("two edge writers successes=%d conflicts=%d", successes, conflicts)
	}
	if got := getFile(t, edgeB, "two-edges", "shared.txt", ""); !bytes.Equal(got, winner) {
		t.Fatalf("second edge did not read winner %q", winner)
	}
}

func TestProtectedSnapshotObjectCannotBeDeletedDuringRetention(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)
	createVolume(t, ts, "object-lock")
	put(t, ts, "object-lock", "clean.txt", []byte("protected clean state"), "")
	resp, err := ts.Client().Post(ts.URL+"/v1/volumes/object-lock/snapshots", "application/json", strings.NewReader(`{"protected":true,"reason":"lock-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("protected snapshot status=%d", resp.StatusCode)
	}
	vol, err := meta.NewStore(pool).GetVolumeByName(context.Background(), "object-lock")
	if err != nil {
		t.Fatal(err)
	}
	var key string
	if err = pool.QueryRow(context.Background(), `SELECT s3_key FROM snapshots WHERE volume_id=$1 AND protected=true`, vol.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if err = mem.Delete(context.Background(), "test-protected", key); err == nil {
		t.Fatal("Object Lock fake allowed deleting a protected object before retention expiry")
	}
	if exists, _ := mem.Exists(context.Background(), "test-protected", key); !exists {
		t.Fatal("protected snapshot disappeared after rejected deletion")
	}
}

func TestRansomwareBurstProtectedSnapshotAndByteForByteRestore(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)
	createVolume(t, ts, "attack")
	clean := map[string][]byte{
		"a.txt": bytes.Repeat([]byte("clean-a-"), 40),
		"b.txt": bytes.Repeat([]byte("clean-b-"), 40),
		"c.txt": bytes.Repeat([]byte("clean-c-"), 40),
	}
	for path, data := range clean {
		put(t, ts, "attack", path, data, "")
	}
	attack := make([]byte, 400)
	for i := range attack {
		attack[i] = byte((i*73 + 19) % 256)
	}
	for path := range clean {
		put(t, ts, "attack", path, attack, "1")
	}
	if got := mem.LockedObjects(); got != 1 {
		t.Fatalf("protected snapshots=%d, want 1", got)
	}
	vol, err := meta.NewStore(pool).GetVolumeByName(context.Background(), "attack")
	if err != nil || !vol.AlertOpen {
		t.Fatalf("alert state=%+v err=%v", vol, err)
	}
	var snapshotID int64
	err = pool.QueryRow(context.Background(), `SELECT id FROM snapshots WHERE volume_id=$1 AND protected=true ORDER BY id DESC LIMIT 1`, vol.ID).Scan(&snapshotID)
	if err != nil {
		t.Fatalf("find protected snapshot: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/volumes/attack/files/other.txt", bytes.NewReader([]byte("blocked")))
	req.Header.Set("X-Edge-Node", "test-edge")
	blocked, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusLocked {
		t.Fatalf("quarantined write status=%d, want 423", blocked.StatusCode)
	}
	body := fmt.Sprintf(`{"snapshot_id":%d}`, snapshotID)
	restored, err := ts.Client().Post(ts.URL+"/v1/volumes/attack/restore", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Body.Close()
	if restored.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(restored.Body)
		t.Fatalf("restore status=%d body=%s", restored.StatusCode, b)
	}
	for path, expected := range clean {
		if got := getFile(t, ts, "attack", path, ""); !bytes.Equal(got, expected) {
			t.Fatalf("restored %s differs byte-for-byte", path)
		}
	}
	vol, err = meta.NewStore(pool).GetVolumeByName(context.Background(), "attack")
	if err != nil || vol.AlertOpen {
		t.Fatalf("alert remains open after recovery: %+v err=%v", vol, err)
	}
}

func TestRansomwareSignalsAreSharedAcrossEdgeReplicas(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	edgeA := newTestServer(t, pool, mem)
	edgeB := newTestServer(t, pool, mem)
	createVolume(t, edgeA, "distributed-attack")
	clean := map[string][]byte{
		"a.txt": bytes.Repeat([]byte("clean-a-"), 40),
		"b.txt": bytes.Repeat([]byte("clean-b-"), 40),
		"c.txt": bytes.Repeat([]byte("clean-c-"), 40),
	}
	paths := []string{"a.txt", "b.txt", "c.txt"}
	for _, path := range paths {
		put(t, edgeA, "distributed-attack", path, clean[path], "")
	}
	attack := make([]byte, 400)
	for i := range attack {
		attack[i] = byte((i*73 + 19) % 256)
	}
	for i, path := range paths {
		edge := edgeA
		if i%2 == 1 {
			edge = edgeB
		}
		put(t, edge, "distributed-attack", path, attack, "1")
	}
	volume, err := meta.NewStore(pool).GetVolumeByName(context.Background(), "distributed-attack")
	if err != nil || !volume.AlertOpen {
		t.Fatalf("cross-edge alert state=%+v err=%v", volume, err)
	}
	if got := mem.LockedObjects(); got != 1 {
		t.Fatalf("protected snapshots=%d, want one shared cross-edge snapshot", got)
	}
	recoveryLease, err := meta.NewStore(pool).AcquireLease(context.Background(), volume.ID, "racing.txt", "edge-b", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = meta.NewStore(pool).Commit(context.Background(), meta.CommitInput{VolumeID: volume.ID, FilePath: "racing.txt", ExpectedVersion: 0, ManifestHash: []byte("blocked"), CreatedBy: "edge-b", LeaseHolder: "edge-b", LeaseToken: recoveryLease.Token})
	if !errors.Is(err, meta.ErrVolumeAlertOpen) {
		t.Fatalf("transaction committed during open alert: %v", err)
	}
	var events int
	if err = pool.QueryRow(context.Background(), `SELECT count(*) FROM ransomware_write_events WHERE volume_id=$1`, volume.ID).Scan(&events); err != nil || events < 6 {
		t.Fatalf("shared detector events=%d err=%v", events, err)
	}
	blocked, _ := http.NewRequest(http.MethodPut, edgeB.URL+"/v1/volumes/distributed-attack/files/blocked.txt", bytes.NewReader([]byte("blocked")))
	blocked.Header.Set("X-Edge-Node", "edge-b")
	blockedResp, err := edgeB.Client().Do(blocked)
	if err != nil {
		t.Fatal(err)
	}
	blockedResp.Body.Close()
	if blockedResp.StatusCode != http.StatusLocked {
		t.Fatalf("other edge write after alert status=%d, want 423", blockedResp.StatusCode)
	}
	var snapshotID int64
	if err = pool.QueryRow(context.Background(), `SELECT id FROM snapshots WHERE volume_id=$1 AND protected=true ORDER BY id DESC LIMIT 1`, volume.ID).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	restoreBody := fmt.Sprintf(`{"snapshot_id":%d}`, snapshotID)
	restored, err := edgeB.Client().Post(edgeB.URL+"/v1/volumes/distributed-attack/restore", "application/json", strings.NewReader(restoreBody))
	if err != nil {
		t.Fatal(err)
	}
	restored.Body.Close()
	if restored.StatusCode != http.StatusOK {
		t.Fatalf("cross-edge restore status=%d", restored.StatusCode)
	}
	for _, path := range paths {
		if got := getFile(t, edgeA, "distributed-attack", path, ""); !bytes.Equal(got, clean[path]) {
			t.Fatalf("restored %s differs byte-for-byte", path)
		}
	}
}

// --- helpers ---

func put(t *testing.T, ts *httptest.Server, vol, path string, data []byte, ifMatch string) map[string]any {
	t.Helper()
	leaseToken := acquireLease(t, ts, vol, path, "test-edge")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/volumes/"+vol+"/files/"+path, bytes.NewReader(data))
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	req.Header.Set("X-Edge-Node", "test-edge")
	req.Header.Set("X-Edge-Lease-Token", fmt.Sprint(leaseToken))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s/%s got %d: %v", vol, path, resp.StatusCode, m)
	}
	return m
}

func acquireLease(t *testing.T, ts *httptest.Server, vol, filePath, holder string) int64 {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/volumes/"+vol+"/files/"+filePath+"/lease", nil)
	req.Header.Set("X-Edge-Node", holder)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("acquire lease got %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token int64 `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Token
}

func TestLeaseFencingRejectsExpiredEdgeAndKeepsConflictCopy(t *testing.T) {
	pool := testDB(t)
	ts := newTestServer(t, pool, blob.NewMemBlob())
	createVolume(t, ts, "fencing")
	oldToken := acquireLease(t, ts, "fencing", "report.txt", "edge-a")
	blocked, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/volumes/fencing/files/report.txt/lease", nil)
	blocked.Header.Set("X-Edge-Node", "edge-b")
	blockedResp, err := ts.Client().Do(blocked)
	if err != nil {
		t.Fatal(err)
	}
	blockedResp.Body.Close()
	if blockedResp.StatusCode != http.StatusLocked {
		t.Fatalf("live lease acquire status=%d, want 423", blockedResp.StatusCode)
	}
	vol, err := meta.NewStore(pool).GetVolumeByName(context.Background(), "fencing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE leases SET expires_at=now()-interval '1 second' WHERE volume_id=$1 AND file_path='report.txt'`, vol.ID); err != nil {
		t.Fatal(err)
	}
	newToken := acquireLease(t, ts, "fencing", "report.txt", "edge-b")
	if newToken <= oldToken {
		t.Fatalf("fencing token did not increase: old=%d new=%d", oldToken, newToken)
	}
	data := []byte("stale edge data")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/volumes/fencing/files/report.txt", bytes.NewReader(data))
	req.Header.Set("X-Edge-Node", "edge-a")
	req.Header.Set("X-Edge-Lease-Token", fmt.Sprint(oldToken))
	req.Header.Set("If-Match", "0")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale writer status=%d body=%s", resp.StatusCode, body)
	}
	conflictPath := resp.Header.Get("X-EdgeVault-Conflict-Path")
	if conflictPath == "" {
		t.Fatal("stale write was not preserved as a conflict copy")
	}
	if got := getFile(t, ts, "fencing", conflictPath, ""); !bytes.Equal(got, data) {
		t.Fatalf("conflict copy bytes=%q want %q", got, data)
	}
	goodReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/volumes/fencing/files/report.txt", bytes.NewReader([]byte("fresh edge data")))
	goodReq.Header.Set("X-Edge-Node", "edge-b")
	goodReq.Header.Set("X-Edge-Lease-Token", fmt.Sprint(newToken))
	goodReq.Header.Set("If-Match", "0")
	goodResp, err := ts.Client().Do(goodReq)
	if err != nil {
		t.Fatal(err)
	}
	goodResp.Body.Close()
	if goodResp.StatusCode != http.StatusCreated {
		t.Fatalf("current fencing token commit status=%d", goodResp.StatusCode)
	}
}

func TestRetentionPolicyRequiresAdminAndAuditsChanges(t *testing.T) {
	pool := testDB(t)
	ts := newTestServer(t, pool, blob.NewMemBlob())
	createVolume(t, ts, "retention")
	call := func(body, token string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/volumes/retention/retention", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	if status, _ := call(`{"keep_days":30,"min_versions":3,"floor_days":7}`, ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", status)
	}
	if status, _ := call(`{"keep_days":6,"min_versions":3,"floor_days":7}`, "integration-admin-token"); status != http.StatusBadRequest {
		t.Fatalf("below-floor status=%d", status)
	}
	if status, b := call(`{"keep_days":20,"min_versions":5,"floor_days":7}`, "integration-admin-token"); status != http.StatusOK {
		t.Fatalf("valid policy status=%d body=%s", status, b)
	}
	vol, err := meta.NewStore(pool).GetVolumeByName(context.Background(), "retention")
	if err != nil {
		t.Fatal(err)
	}
	if vol.KeepDays != 20 || vol.MinVersions != 5 || vol.FloorDays != 7 {
		t.Fatalf("stored policy=%+v", vol)
	}
	var audits int
	if err = pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE volume_id=$1 AND kind='retention_policy_changed'`, vol.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("retention audit count=%d err=%v", audits, err)
	}
}

func seedGCVolume(t *testing.T, pool *pgxpool.Pool, ts *httptest.Server, volName string) (int64, map[int][]byte) {
	t.Helper()
	createVolume(t, ts, volName)
	data := map[int][]byte{}
	for version := 1; version <= 5; version++ {
		payload := bytes.Repeat([]byte(fmt.Sprintf("version-%d-", version)), 30)
		data[version] = payload
		match := ""
		if version > 1 {
			match = fmt.Sprint(version - 1)
		}
		put(t, ts, volName, "history.bin", payload, match)
		if version == 1 {
			resp, err := ts.Client().Post(ts.URL+"/v1/volumes/"+volName+"/snapshots", "application/json", strings.NewReader(`{"protected":true,"reason":"gc-test"}`))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("protected snapshot status=%d", resp.StatusCode)
			}
		}
	}
	vol, err := meta.NewStore(pool).GetVolumeByName(context.Background(), volName)
	if err != nil {
		t.Fatal(err)
	}
	if err = meta.NewStore(pool).SetRetentionPolicy(context.Background(), vol.ID, "test", 7, 3, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE file_versions SET created_at=now()-interval '90 days' WHERE volume_id=$1`, vol.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE chunks SET last_used_at=now()-interval '90 days' WHERE volume_id=$1`, vol.ID); err != nil {
		t.Fatal(err)
	}
	return vol.ID, data
}

func TestGCRespectsMinVersionsAndProtectedSnapshot(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)
	volumeID, data := seedGCVolume(t, pool, ts, "gc-retention")
	store := meta.NewStore(pool)
	file, err := store.GetFile(context.Background(), volumeID, "history.bin")
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := store.GetVersionChunks(context.Background(), file.ID, 2)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("v2 chunks=%v err=%v", chunks, err)
	}
	chunkMeta, err := store.GetChunk(context.Background(), volumeID, chunks[0].ChunkID)
	if err != nil {
		t.Fatal(err)
	}
	runner := gcengine.Runner{Store: store, Blob: mem, Bucket: "test-bucket"}
	dry, err := runner.Run(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.VersionsExpired != 1 || dry.ChunksConsidered != 1 || dry.ChunksDeleted != 0 {
		t.Fatalf("dry run report=%+v", dry)
	}
	if _, err = store.GetFileVersion(context.Background(), file.ID, 2); err != nil {
		t.Fatalf("dry run removed v2: %v", err)
	}
	actual, err := runner.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if actual.VersionsExpired != 1 || actual.ChunksDeleted != 1 {
		t.Fatalf("gc report=%+v", actual)
	}
	if exists, _ := mem.Exists(context.Background(), "test-bucket", chunkMeta.S3Key); exists {
		t.Fatal("expired v2 chunk still exists")
	}
	for _, version := range []int{1, 3, 4, 5} {
		got := getFile(t, ts, "gc-retention", "history.bin", fmt.Sprint(version))
		if !bytes.Equal(got, data[version]) {
			t.Fatalf("retained v%d changed after GC", version)
		}
	}
	if _, err = store.GetFileVersion(context.Background(), file.ID, 2); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("expired v2 metadata still exists: %v", err)
	}
}

func TestGCPropertyKeepsEveryProtectedAndNewestVersion(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)
	createVolume(t, ts, "gc-property")
	store := meta.NewStore(pool)
	volume, err := store.GetVolumeByName(context.Background(), "gc-property")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRetentionPolicy(context.Background(), volume.ID, "test", 7, 3, 7); err != nil {
		t.Fatal(err)
	}
	paths := []string{"alpha.bin", "beta.bin", "gamma.bin"}
	const versionsPerFile = 6
	expected := make(map[string]map[int][]byte, len(paths))
	for _, p := range paths {
		expected[p] = make(map[int][]byte)
	}
	versions := make(map[string]int)
	var protectedSeqs []int64
	for version := 1; version <= versionsPerFile; version++ {
		for _, p := range paths {
			data := []byte(fmt.Sprintf("%s-property-v%d-", p, version) + strings.Repeat(string(rune('A'+version)), 80))
			match := ""
			if versions[p] > 0 {
				match = fmt.Sprint(versions[p])
			}
			result := put(t, ts, "gc-property", p, data, match)
			versions[p] = int(result["version"].(float64))
			expected[p][versions[p]] = data
		}
		if version == 1 || version == 3 || version == 5 {
			resp, e := ts.Client().Post(ts.URL+"/v1/volumes/gc-property/snapshots", "application/json", strings.NewReader(`{"protected":true,"reason":"gc-property"}`))
			if e != nil {
				t.Fatal(e)
			}
			var snapshot struct {
				CommitSeq int64 `json:"commit_seq"`
			}
			if e = json.NewDecoder(resp.Body).Decode(&snapshot); e != nil {
				resp.Body.Close()
				t.Fatal(e)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("snapshot status=%d", resp.StatusCode)
			}
			protectedSeqs = append(protectedSeqs, snapshot.CommitSeq)
		}
	}
	retained := make(map[string]map[int][]byte, len(paths))
	for _, p := range paths {
		retained[p] = make(map[int][]byte)
	}
	for _, seq := range protectedSeqs {
		entries, e := store.StateAt(context.Background(), volume.ID, seq, "")
		if e != nil {
			t.Fatal(e)
		}
		for _, entry := range entries {
			data, ok := expected[entry.Path][int(entry.Version)]
			if !ok {
				t.Fatalf("model missing %s version %d at protected seq %d", entry.Path, entry.Version, seq)
			}
			retained[entry.Path][int(entry.Version)] = data
		}
	}
	for path := range expected {
		for version := versions[path] - 2; version <= versions[path]; version++ {
			data, ok := expected[path][version]
			if !ok {
				t.Fatalf("model missing newest %s version %d", path, version)
			}
			retained[path][version] = data
		}
	}
	if _, err = pool.Exec(context.Background(), `UPDATE file_versions SET created_at=now()-interval '90 days' WHERE volume_id=$1`, volume.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE chunks SET last_used_at=now()-interval '90 days' WHERE volume_id=$1`, volume.ID); err != nil {
		t.Fatal(err)
	}
	report, err := (&gcengine.Runner{Store: store, Blob: mem, Bucket: "test-bucket"}).Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if report.VersionsExpired == 0 || report.ChunksDeleted == 0 {
		t.Fatalf("property fixture did not exercise collection: %+v", report)
	}
	for p, versions := range retained {
		for version, want := range versions {
			got := getFile(t, ts, "gc-property", p, fmt.Sprint(version))
			if !bytes.Equal(got, want) {
				t.Fatalf("GC removed or changed retained %s version %d", p, version)
			}
		}
	}
}

type blockingDeleter struct {
	mem     *blob.MemBlob
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockingDeleter) Delete(ctx context.Context, bucket, key string) error {
	d.once.Do(func() { close(d.started) })
	select {
	case <-d.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return d.mem.Delete(ctx, bucket, key)
}

func TestUploadRacingGCReuploadsCollectedChunk(t *testing.T) {
	pool := testDB(t)
	mem := blob.NewMemBlob()
	ts := newTestServer(t, pool, mem)
	_, data := seedGCVolume(t, pool, ts, "gc-race")
	// Acquire the lease before GC takes the chunk row lock. Some test databases
	// use a single connection, so doing DB work while the deleter is paused
	// would prevent the test from releasing that pause.
	token := acquireLease(t, ts, "gc-race", "arriving-copy.bin", "writer-new")
	deleter := &blockingDeleter{mem: mem, started: make(chan struct{}), release: make(chan struct{})}
	runner := gcengine.Runner{Store: meta.NewStore(pool), Blob: deleter, Bucket: "test-bucket"}
	gcDone := make(chan error, 1)
	go func() { _, err := runner.Run(context.Background(), false); gcDone <- err }()
	select {
	case <-deleter.started:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach stale object deletion")
	}
	uploadDone := make(chan *http.Response, 1)
	uploadErr := make(chan error, 1)
	payload := data[2]
	go func() {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/v1/volumes/gc-race/files/arriving-copy.bin", bytes.NewReader(payload))
		req.Header.Set("X-Edge-Node", "writer-new")
		req.Header.Set("X-Edge-Lease-Token", fmt.Sprint(token))
		req.Header.Set("If-Match", "0")
		resp, err := ts.Client().Do(req)
		if err != nil {
			uploadErr <- err
			return
		}
		uploadDone <- resp
	}()
	time.Sleep(50 * time.Millisecond)
	close(deleter.release)
	if err := <-gcDone; err != nil {
		t.Fatalf("gc run: %v", err)
	}
	select {
	case err := <-uploadErr:
		t.Fatal(err)
	case resp := <-uploadDone:
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("racing upload status=%d body=%s", resp.StatusCode, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("racing upload did not finish")
	}
	if got := getFile(t, ts, "gc-race", "arriving-copy.bin", ""); !bytes.Equal(got, payload) {
		t.Fatal("upload racing GC lost data")
	}
}

func getFile(t *testing.T, ts *httptest.Server, vol, path, version string) []byte {
	t.Helper()
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	url := ts.URL + "/v1/volumes/" + vol + "/files/" + strings.Join(parts, "/")
	if version != "" {
		url += "?version=" + version
	}
	resp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s got %d: %s", url, resp.StatusCode, b)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}
