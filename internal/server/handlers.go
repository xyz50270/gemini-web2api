package server

import (
	"net/http"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}

	modelNames := make([]string, 0, len(models.MODELS))
	for k := range models.MODELS {
		modelNames = append(modelNames, k)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": a.Version,
		"models":  modelNames,
	})
}

func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]any
	for name, m := range models.MODELS {
		data = append(data, map[string]any{
			"id":          name,
			"object":      "model",
			"created":     1700000000,
			"owned_by":    "google",
			"description": m.Desc,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (a *App) handleGoogleModels(w http.ResponseWriter, r *http.Request) {
	var modelList []map[string]any
	for name, m := range models.MODELS {
		modelList = append(modelList, map[string]any{
			"name":                       "models/" + name,
			"displayName":                name,
			"description":                m.Desc,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models": modelList,
	})
}

func (a *App) handleAccounts(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	threshold := a.Cfg.QuotaThreshold
	if threshold <= 0 || threshold > 1 {
		threshold = 0.95
	}

	snapshots := make([]ExhaustedAccount, 0, a.Pool.Len())
	for _, acct := range a.Pool.Accounts() {
		snapshots = append(snapshots, acct.Snapshot(threshold, now))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object":            "list",
		"quota_threshold":   threshold,
		"quota_refresh_sec": a.Cfg.QuotaRefreshSec,
		"accounts":          snapshots,
	})
}
