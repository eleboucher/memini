package apiauth_test

import (
	"testing"

	"github.com/eleboucher/memini/internal/apiauth"
)

// TestGenerateSecretShape pins the one canonical secret generator's shape:
// 32 random bytes hex-encoded to 64 characters, matching what the CLI (K3)
// and the REST key-management API (K3b) both hand back to the caller.
func TestGenerateSecretShape(t *testing.T) {
	s, err := apiauth.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if len(s) != 64 {
		t.Fatalf("want 64 hex chars (32 random bytes), got %d: %q", len(s), s)
	}
}

func TestGenerateSecretNoCollision(t *testing.T) {
	a, err := apiauth.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	b, err := apiauth.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if a == b {
		t.Fatal("two generated secrets should not collide")
	}
}
