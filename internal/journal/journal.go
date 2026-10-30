// Package journal stores durable upload intents. Each record is synced before
// a client write can proceed; ACK records make replay idempotent after restart.
package journal

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type Record struct {
	ID     string `json:"id"`
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Data   []byte `json:"data"`
}
type diskRecord struct {
	ID     string `json:"id"`
	Bucket string `json:"bucket,omitempty"`
	Key    string `json:"key,omitempty"`
	Data   string `json:"data,omitempty"`
	Ack    bool   `json:"ack,omitempty"`
}
type Journal struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func Open(path string) (*Journal, []Record, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, nil, err
	}
	j := &Journal{f: f, path: path}
	pending, err := j.Replay()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return j, pending, nil
}

func (j *Journal) Append(r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if r.ID == "" || r.Key == "" {
		return errors.New("journal record needs id and key")
	}
	d := diskRecord{ID: r.ID, Bucket: r.Bucket, Key: r.Key, Data: base64.StdEncoding.EncodeToString(r.Data)}
	if err := json.NewEncoder(j.f).Encode(d); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *Journal) Ack(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := json.NewEncoder(j.f).Encode(diskRecord{ID: id, Ack: true}); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *Journal) Replay() ([]Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Seek(0, 0); err != nil {
		return nil, err
	}
	pending := map[string]Record{}
	scanner := bufio.NewScanner(j.f)
	scanner.Buffer(make([]byte, 4096), 64<<20)
	for scanner.Scan() {
		var d diskRecord
		if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
			return nil, err
		}
		if d.Ack {
			delete(pending, d.ID)
			continue
		}
		b, err := base64.StdEncoding.DecodeString(d.Data)
		if err != nil {
			return nil, err
		}
		pending[d.ID] = Record{ID: d.ID, Bucket: d.Bucket, Key: d.Key, Data: b}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if _, err := j.f.Seek(0, 2); err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(pending))
	for _, r := range pending {
		out = append(out, r)
	}
	return out, nil
}
func (j *Journal) Close() error { j.mu.Lock(); defer j.mu.Unlock(); return j.f.Close() }
