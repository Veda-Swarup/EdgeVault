package cache

import (
	"errors"
	"testing"
)

func TestLRUEvictsCleanAndPinsDirty(t *testing.T) {
	c, err := Open(t.TempDir(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Put("dirty", []byte("1234"), true); err != nil {
		t.Fatal(err)
	}
	if err = c.Put("clean", []byte("a"), false); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get("dirty"); err != nil {
		t.Fatal(err)
	} // make dirty most recent
	if err = c.Put("next", []byte("b"), false); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Get("dirty"); err != nil {
		t.Fatal("dirty entry was evicted:", err)
	}
	if _, err = c.Get("clean"); !errors.Is(err, ErrMiss) {
		t.Fatal("least-recent clean entry should be evicted")
	}
	if err = c.Put("too-big", []byte("123456"), false); err == nil {
		t.Fatal("expected capacity error")
	}
}
