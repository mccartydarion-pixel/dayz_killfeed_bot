package repository

import (
	"errors"
	"testing"
)

func TestNormalizeDiscordInvite(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"https://discord.gg/deadzone", "https://discord.gg/deadzone", true},
		{"discord.gg/deadzone", "https://discord.gg/deadzone", true},
		{" http://www.discord.gg/Dead-Zone9/ ", "https://discord.gg/Dead-Zone9", true},
		{"https://discord.gg/invite/abc123", "https://discord.gg/abc123", true},
		{"https://discord.com/invite/abc123?event=42", "https://discord.gg/abc123", true},
		{"https://discordapp.com/invite/abc123", "https://discord.gg/abc123", true},
		{"https://DISCORD.GG/abc123", "https://discord.gg/abc123", true},
		{"https://discord.com/channels/1/2", "", false},
		{"https://discord.gg/", "", false},
		{"https://discord.gg/a", "", false},
		{"https://discord.gg/abc/def", "", false},
		{"https://discord.gg.evil.example/abc123", "", false},
		{"https://evil.example/discord.gg/abc123", "", false},
		{"https://evil.example/?u=https://discord.gg/abc123", "", false},
		{"javascript:alert(1)//discord.gg/abc123", "", false},
		{"https://discord.gg/abc 123", "", false},
		{"https://discord.gg/abc123\nhttps://evil.example", "", false},
	} {
		got, ok := NormalizeDiscordInvite(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeDiscordInvite(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNetworkSettingsInviteValidation(t *testing.T) {
	s := NetworkSettings{Listed: true, DiscordInviteURL: " discord.com/invite/abc123 "}.Normalize()
	if s.DiscordInviteURL != "https://discord.gg/abc123" {
		t.Fatalf("normalized invite = %q", s.DiscordInviteURL)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("valid invite rejected: %v", err)
	}
	if err := (NetworkSettings{}).Normalize().Validate(); err != nil {
		t.Fatalf("empty invite rejected: %v", err)
	}
	bad := NetworkSettings{DiscordInviteURL: "https://example.com/join"}.Normalize()
	if err := bad.Validate(); !errors.Is(err, ErrInvalidFeatureSettings) {
		t.Fatalf("arbitrary URL accepted: %v", err)
	}
}
