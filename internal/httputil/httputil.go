// Package httputil holds tiny HTTP helpers shared across the REST and
// /healthz handlers.
package httputil

import (
	"encoding/json"
	"net/http"
)

// JSON writes status and body as application/json, ignoring the encoder's
// write error (the response status has already been sent).
func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Error writes a {"error": msg} JSON body with the given status.
func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}
