package journal

import (
	"bytes"
	"testing"
)

func TestReplayPendingAndAck(t *testing.T) {
	path := t.TempDir() + "/state/journal.jsonl"
	j, _, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := Record{ID: "id1", Bucket: "b", Key: "k", Data: []byte("payload")}
	if err = j.Append(r); err != nil {
		t.Fatal(err)
	}
	pending, err := j.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !bytes.Equal(pending[0].Data, r.Data) {
		t.Fatalf("pending=%+v", pending)
	}
	if err = j.Ack(r.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = j.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected empty journal, got %d", len(pending))
	}
	if err = j.Close(); err != nil {
		t.Fatal(err)
	}
}
