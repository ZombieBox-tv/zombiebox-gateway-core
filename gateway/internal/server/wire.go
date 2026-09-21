// Package server exposes the versioned client protocol. Provider DTOs stay here.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
)

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func tokenHash(token string) string {
	v := sha256.Sum256([]byte(token))
	return hex.EncodeToString(v[:])
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, code string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code}, "apiVersion": 1})
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil {
		fail(w, 400, "invalid_json")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		fail(w, 400, "invalid_json")
		return false
	}
	return true
}
