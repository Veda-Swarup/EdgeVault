package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vedaswarup/edgevault/internal/blob"
)

func TestDashboardReadModel(t *testing.T) {
	pool := testDB(t)
	ts := newTestServer(t, pool, blob.NewMemBlob())
	createVolume(t, ts, "console-test")
	first := bytes.Repeat([]byte("A"), 200)
	put(t, ts, "console-test", "report.txt", first, "")
	second := append(bytes.Repeat([]byte("A"), 100), bytes.Repeat([]byte("B"), 100)...)
	put(t, ts, "console-test", "report.txt", second, "1")
	// Another file shares the same chunks; it must not double stored bytes.
	put(t, ts, "console-test", "copy.txt", second, "")
	res, err := ts.Client().Post(ts.URL+"/v1/volumes/console-test/snapshots", "application/json", strings.NewReader(`{"reason":"console test"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot returned %d", res.StatusCode)
	}
	res, err = ts.Client().Get(ts.URL + "/v1/volumes")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || strings.Contains(string(body), "wrapped") || strings.Contains(string(body), "kms") {
		t.Fatalf("unsafe volume response: %s", body)
	}
	var listing struct {
		Volumes []struct {
			Name string `json:"name"`
		} `json:"volumes"`
	}
	if err = json.Unmarshal(body, &listing); err != nil || len(listing.Volumes) != 1 || listing.Volumes[0].Name != "console-test" {
		t.Fatalf("invalid listing: %s", body)
	}
	res, err = ts.Client().Get(ts.URL + "/v1/volumes/console-test/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("dashboard returned %d", res.StatusCode)
	}
	var data struct {
		Storage struct {
			CurrentBytes int64 `json:"current_bytes"`
			StoredBytes  int64 `json:"stored_bytes"`
			ChunkCount   int64 `json:"chunk_count"`
			VersionCount int64 `json:"version_count"`
			Files        []struct {
				Path string `json:"path"`
			} `json:"files"`
			Snapshots []struct {
				Reason string `json:"reason"`
			} `json:"snapshots"`
		} `json:"storage"`
	}
	if err = json.NewDecoder(res.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	s := data.Storage
	if s.CurrentBytes != 400 || s.ChunkCount != 2 || s.VersionCount != 3 || len(s.Files) != 2 || len(s.Snapshots) != 1 || s.Snapshots[0].Reason != "console test" || s.StoredBytes <= 0 || s.StoredBytes >= 400 {
		t.Fatalf("unexpected dashboard: %+v", s)
	}
}
