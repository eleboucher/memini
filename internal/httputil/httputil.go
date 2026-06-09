// Package httputil holds tiny HTTP helpers shared across the REST and
// /healthz handlers.
package httputil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// ValidateNamespace returns a non-nil error when ns is not a valid namespace.
// Valid namespaces are 1–256 bytes, contain no NUL character.
func ValidateNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("namespace is empty")
	}
	if len(ns) > 256 {
		return fmt.Errorf("namespace exceeds 256 bytes")
	}
	if strings.ContainsRune(ns, 0) {
		return fmt.Errorf("namespace contains NUL byte")
	}
	return nil
}
