package uploader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/vedaswarup/edgevault/internal/blob"
	"github.com/vedaswarup/edgevault/internal/cache"
	"github.com/vedaswarup/edgevault/internal/journal"
	"github.com/vedaswarup/edgevault/internal/metrics"
)

type job struct {
	rec  journal.Record
	done chan error
}
type Uploader struct {
	bl    blob.Blob
	cache *cache.Cache
	j     *journal.Journal
	q     chan job
	m     *metrics.Metrics
	wg    sync.WaitGroup
}

func New(ctx context.Context, bl blob.Blob, c *cache.Cache, j *journal.Journal, replay []journal.Record, m *metrics.Metrics, workers int) *Uploader {
	if workers < 1 {
		workers = 1
	}
	u := &Uploader{bl: bl, cache: c, j: j, q: make(chan job, workers*4), m: m}
	u.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go u.worker(ctx)
	}
	for _, r := range replay {
		if err := c.Put(r.Bucket+"/"+r.Key, r.Data, true); err == nil {
			m.DirtyBytes.Add(int64(len(r.Data)))
		}
		u.q <- job{rec: r}
		m.UploadQueueDepth.Add(1)
	}
	return u
}

func RecordID(bucket, key string, data []byte) string {
	sum := sha256.Sum256(append([]byte(bucket+"\x00"+key+"\x00"), data...))
	return hex.EncodeToString(sum[:])
}

// Submit persists the intent before returning, then waits until S3 has the object.
func (u *Uploader) Submit(ctx context.Context, bucket, key string, data []byte) error {
	id := RecordID(bucket, key, data)
	r := journal.Record{ID: id, Bucket: bucket, Key: key, Data: append([]byte(nil), data...)}
	if err := u.j.Append(r); err != nil {
		return err
	}
	if err := u.cache.Put(bucket+"/"+key, data, true); err != nil {
		return err
	}
	u.m.DirtyBytes.Add(int64(len(data)))
	done := make(chan error, 1)
	select {
	case u.q <- job{rec: r, done: done}:
		u.m.UploadQueueDepth.Add(1)
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (u *Uploader) worker(ctx context.Context) {
	defer u.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-u.q:
			err := u.bl.Put(ctx, j.rec.Bucket, j.rec.Key, bytes.NewReader(j.rec.Data), int64(len(j.rec.Data)))
			if err == nil {
				err = u.j.Ack(j.rec.ID)
			}
			if err == nil {
				u.cache.MarkClean(j.rec.Bucket + "/" + j.rec.Key)
				u.m.DirtyBytes.Add(-int64(len(j.rec.Data)))
			} else {
				u.m.DirtyBytes.Add(0)
			}
			u.m.UploadQueueDepth.Add(-1)
			if j.done != nil {
				j.done <- err
			}
		}
	}
}

func (u *Uploader) Close() { u.wg.Wait() }

func Fetch(ctx context.Context, bl blob.Blob, c *cache.Cache, m *metrics.Metrics, bucket, key string) ([]byte, bool, error) {
	start := time.Now()
	if b, err := c.Get(bucket + "/" + key); err == nil {
		m.CacheHits.Add(1)
		m.CacheReadLatencySum.Add(int64(time.Since(start)))
		m.CacheReadLatencyCount.Add(1)
		return b, true, nil
	} else if !errors.Is(err, cache.ErrMiss) {
		return nil, false, err
	}
	m.CacheMisses.Add(1)
	r, err := bl.Get(ctx, bucket, key)
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, false, err
	}
	_ = c.Put(bucket+"/"+key, b, false)
	m.S3ReadLatencySum.Add(int64(time.Since(start)))
	m.S3ReadLatencyCount.Add(1)
	return b, false, nil
}
