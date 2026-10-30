// Package detector scores short write bursts with entropy jumps against each
// file's prior version. It intentionally keeps only a bounded 60-second window.
package detector

import (
	"sync"
	"time"
)

const (
	Window           = 60 * time.Second
	MinDistinctFiles = 3
	MinEntropyJump   = 2.0
	MinJumpedFiles   = 2
)

type event struct {
	path      string
	beforeSeq int64
	jumped    bool
	at        time.Time
}
type Decision struct {
	Alert            bool
	BaselineSeq      int64
	Score            float64
	DistinctFiles    int
	EntropyJumpFiles int
}
type Detector struct {
	mu      sync.Mutex
	windows map[int64][]event
	alerted map[int64]bool
}

func New() *Detector {
	return &Detector{windows: make(map[int64][]event), alerted: make(map[int64]bool)}
}

func (d *Detector) Observe(volumeID int64, path string, beforeSeq int64, previous, current *float32, now time.Time) Decision {
	d.mu.Lock()
	defer d.mu.Unlock()
	events := d.windows[volumeID]
	cutoff := now.Add(-Window)
	first := 0
	for first < len(events) && events[first].at.Before(cutoff) {
		first++
	}
	if first == len(events) {
		delete(d.alerted, volumeID)
	}
	events = append([]event(nil), events[first:]...)
	jumped := false
	if previous != nil && current != nil && float64(*current-*previous) >= MinEntropyJump {
		jumped = true
	}
	// The first entropy jump marks the start of a likely attack burst. Discard
	// earlier ordinary writes so the protected baseline includes the clean state.
	if jumped {
		hasJump := false
		for _, e := range events {
			if e.jumped {
				hasJump = true
				break
			}
		}
		if !hasJump {
			events = nil
		}
	}
	events = append(events, event{path: path, beforeSeq: beforeSeq, jumped: jumped, at: now})
	d.windows[volumeID] = events
	distinct := map[string]bool{}
	jumpFiles := map[string]bool{}
	baseline := events[0].beforeSeq
	for _, e := range events {
		distinct[e.path] = true
		if e.jumped {
			jumpFiles[e.path] = true
		}
	}
	if len(events) > 0 {
		baseline = events[0].beforeSeq
	}
	score := float64(len(distinct)) + 2*float64(len(jumpFiles))
	alert := len(distinct) >= MinDistinctFiles && len(jumpFiles) >= MinJumpedFiles && !d.alerted[volumeID]
	if alert {
		d.alerted[volumeID] = true
	}
	if len(events) == 0 {
		delete(d.alerted, volumeID)
	}
	return Decision{Alert: alert, BaselineSeq: baseline, Score: score, DistinctFiles: len(distinct), EntropyJumpFiles: len(jumpFiles)}
}

func (d *Detector) Clear(volumeID int64) {
	d.mu.Lock()
	delete(d.windows, volumeID)
	delete(d.alerted, volumeID)
	d.mu.Unlock()
}
