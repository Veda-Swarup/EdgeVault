package uploader

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/vedaswarup/edgevault/internal/blob"
	"github.com/vedaswarup/edgevault/internal/cache"
	"github.com/vedaswarup/edgevault/internal/journal"
	"github.com/vedaswarup/edgevault/internal/metrics"
)

type countedBlob struct {
	*blob.MemBlob
	gets atomic.Int64
}

func (b *countedBlob) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	b.gets.Add(1)
	return b.MemBlob.Get(ctx, bucket, key)
}

func TestSubmitAndWarmReadAvoidsBlobGet(t *testing.T) {
	bl := &countedBlob{MemBlob: blob.NewMemBlob()}
	c, err := cache.Open(t.TempDir()+"/cache", 1024)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := journal.Open(t.TempDir() + "/journal.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx, cancel := context.WithCancel(context.Background())
	m := &metrics.Metrics{}
	u := New(ctx, bl, c, j, nil, m, 1)
	defer func() { cancel(); u.Close() }()
	if err = u.Submit(context.Background(), "bucket", "key", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	got, hit, err := Fetch(context.Background(), bl, c, m, "bucket", "key")
	if err != nil {
		t.Fatal(err)
	}
	if !hit || string(got) != "payload" {
		t.Fatalf("hit=%v data=%q", hit, got)
	}
	if bl.gets.Load() != 0 {
		t.Fatalf("warm read made %d blob gets", bl.gets.Load())
	}
	if m.CacheHits.Load() != 1 {
		t.Fatalf("cache hits=%d", m.CacheHits.Load())
	}
}
