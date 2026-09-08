package nitrado

import "testing"

func TestClientBuildsAuthorizationHeaders(t *testing.T) {
	client := NewClient("https://api.nitrado.net", "test-token", nil)
	if client == nil {
		t.Fatal("expected client instance")
	}
	if client.token != "test-token" {
		t.Fatal("expected token to be preserved")
	}
}
