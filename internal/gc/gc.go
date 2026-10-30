package gc

import (
	"context"
	"fmt"
	"time"

	"github.com/vedaswarup/edgevault/internal/blob"
	"github.com/vedaswarup/edgevault/internal/meta"
)

const GracePeriod = 24 * time.Hour

type Report struct {
	Volumes          int   `json:"volumes"`
	AlertSkipped     int   `json:"alert_skipped"`
	VersionsExpired  int64 `json:"versions_expired"`
	ChunksConsidered int64 `json:"chunks_considered"`
	ChunksDeleted    int64 `json:"chunks_deleted"`
}
type Runner struct {
	Store  *meta.Store
	Blob   blob.Deleter
	Bucket string
	Now    func() time.Time
}

func (r *Runner) Run(ctx context.Context, dryRun bool) (Report, error) {
	if r.Store == nil {
		return Report{}, fmt.Errorf("gc: metadata store is required")
	}
	if !dryRun && (r.Blob == nil || r.Bucket == "") {
		return Report{}, fmt.Errorf("gc: deleter and chunk bucket are required")
	}
	volumes, err := r.Store.ListVolumes(ctx)
	if err != nil {
		return Report{}, err
	}
	report := Report{Volumes: len(volumes)}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	cutoff := now.Add(-GracePeriod)
	for _, vol := range volumes {
		versions, skipped, err := r.Store.ExpireOldVersions(ctx, vol.ID, dryRun)
		if err != nil {
			return report, err
		}
		report.VersionsExpired += versions
		if skipped {
			report.AlertSkipped++
			continue
		}
		candidates, err := r.Store.ListStaleChunks(ctx, vol.ID, cutoff)
		if err != nil {
			return report, err
		}
		for _, chunk := range candidates {
			report.ChunksConsidered++
			deleted, err := r.Store.SweepChunk(ctx, vol.ID, chunk.ChunkID, cutoff, dryRun, func(ctx context.Context, key string) error { return r.Blob.Delete(ctx, r.Bucket, key) })
			if err != nil {
				return report, err
			}
			if deleted && !dryRun {
				report.ChunksDeleted++
			}
		}
	}
	return report, nil
}
