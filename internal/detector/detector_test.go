package detector

import (
	"testing"
	"time"
)

func fp(v float32) *float32 { return &v }

func TestWriteBurstWithEntropyJumpsAlertsAtPreWindowSequence(t *testing.T) {
	d := New()
	now := time.Now()
	for i, p := range []string{"a", "b", "c"} {
		got := d.Observe(1, p, int64(10+i), fp(1), fp(4), now.Add(time.Duration(i)*time.Second))
		if i < 2 && got.Alert {
			t.Fatal("alerted before threshold")
		}
		if i == 2 {
			if !got.Alert || got.BaselineSeq != 10 || got.DistinctFiles != 3 || got.EntropyJumpFiles != 3 {
				t.Fatalf("decision=%+v", got)
			}
		}
	}
}

func TestAlreadyCompressedContentDoesNotAlertAndWindowExpires(t *testing.T) {
	d := New()
	now := time.Now()
	for i, p := range []string{"a", "b", "c"} {
		got := d.Observe(2, p, int64(i), fp(7.8), fp(7.9), now.Add(time.Duration(i)*time.Second))
		if got.Alert {
			t.Fatal("small entropy change caused alert")
		}
	}
	got := d.Observe(2, "d", 4, fp(1), fp(5), now.Add(2*Window))
	if got.Alert {
		t.Fatal("old writes should have expired")
	}
	if got.DistinctFiles != 1 {
		t.Fatalf("events retained outside window: %+v", got)
	}
}
