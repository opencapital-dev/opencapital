package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/portfolio-management/control-plane/internal/manifest"
)

type sourceDTO struct {
	ManifestURL string `json:"manifest_url"`
	Publisher   string `json:"publisher"`
	Enabled     bool   `json:"enabled"`
}

type addSourceRequest struct {
	ManifestURL string `json:"manifest_url"`
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.store.ListPluginSources(ctx)
	if err != nil {
		s.logger.Error("sources: list", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	out := make([]sourceDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, sourceDTO{p.ManifestURL, p.Publisher, p.Enabled})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	var req addSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ManifestURL == "" {
		http.Error(w, "manifest_url required", http.StatusBadRequest)
		return
	}
	if u, perr := url.Parse(req.ManifestURL); perr != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "manifest_url must be an http(s) URL", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	m, err := manifest.NewPluginClient(req.ManifestURL, nil, manifest.DefaultTTL, s.logger).Fetch(ctx)
	if err != nil {
		http.Error(w, "manifest unreachable or invalid: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.store.CreatePluginSource(ctx, req.ManifestURL, m.Publisher); err != nil {
		http.Error(w, "source already added or store error", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusCreated, sourceDTO{req.ManifestURL, m.Publisher, true})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("manifest_url")
	if url == "" {
		http.Error(w, "manifest_url query param required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	deleted, err := s.store.DeletePluginSource(ctx, url)
	if err != nil {
		s.logger.Error("sources: delete", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, "source not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
