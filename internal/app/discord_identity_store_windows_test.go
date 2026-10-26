//go:build windows

package app

import (
	"bytes"
	"testing"
)

func TestDiscordIdentityDPAPIProtectionRoundTrip(t *testing.T) {
	token := []byte("a-test-worker-signed-identity-token")
	protected, err := protectDiscordIdentity(token)
	if err != nil {
		t.Fatalf("protectDiscordIdentity: %v", err)
	}
	if bytes.Equal(protected, token) {
		t.Fatal("protected identity is plaintext")
	}
	plain, err := unprotectDiscordIdentity(protected)
	if err != nil {
		t.Fatalf("unprotectDiscordIdentity: %v", err)
	}
	if !bytes.Equal(plain, token) {
		t.Fatalf("unprotected token = %q, want original token", plain)
	}
}
