// Package server implements the bootstrap HTTP surface, not the full protocol.
package server

import (
	"encoding/json"
	"net/http"
)

type Health struct {
	Status     string `json:"status"`
	APIVersion int    `json:"apiVersion"`
}

func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(Health{Status: "ok", APIVersion: 1})
	})
	return mux
}
