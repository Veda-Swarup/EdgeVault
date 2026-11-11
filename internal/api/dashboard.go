package api

import (
	"net/http"
	"strings"

	"github.com/vedaswarup/edgevault/internal/meta"
)

// Public volume summaries deliberately omit wrapped keys and KMS identifiers.
func volumeSummary(v meta.Volume) map[string]any {
	return map[string]any{"name": v.Name, "head_seq": v.HeadSeq, "alert_open": v.AlertOpen, "keep_days": v.KeepDays, "min_versions": v.MinVersions, "floor_days": v.FloorDays}
}

func (h *Handler) handleVolumes(w http.ResponseWriter, r *http.Request) {
	volumes, err := h.store.ListVolumes(r.Context())
	if err != nil {
		writeError(w, 500, "could not list volumes")
		return
	}
	out := make([]map[string]any, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, volumeSummary(v))
	}
	writeJSON(w, 200, map[string]any{"volumes": out})
}

func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/volumes/"), "/dashboard")
	vol, ok := h.resolveVolume(r.Context(), w, name)
	if !ok {
		return
	}
	data, err := h.store.Dashboard(r.Context(), vol.ID)
	if err != nil {
		h.log.Error("dashboard", "err", err)
		writeError(w, 500, "could not load dashboard")
		return
	}
	writeJSON(w, 200, map[string]any{"volume": volumeSummary(vol), "storage": data})
}
