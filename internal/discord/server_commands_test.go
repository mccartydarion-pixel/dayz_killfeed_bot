package discord

import (
	"errors"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func TestModalValueExtractsTextInputByCustomID(t *testing.T) {
	data := discordgo.ModalSubmitInteractionData{
		CustomID: serverConnectModalID,
		Components: []discordgo.MessageComponent{
			&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				&discordgo.TextInput{CustomID: serverConnectTokenID, Value: "  secret-token  "},
			}},
		},
	}
	if got := modalValue(data, serverConnectTokenID); got != "  secret-token  " {
		t.Fatalf("expected raw value returned (trimming is the caller's job), got %q", got)
	}
	if got := modalValue(data, "missing"); got != "" {
		t.Fatalf("expected empty string for unknown custom ID, got %q", got)
	}
}

func TestClassifyNitradoErrNeverLeaksDetail(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"auth", &nitrado.RequestError{Kind: nitrado.KindAuthentication, StatusCode: 401}, "invalid or expired token"},
		{"permission", &nitrado.RequestError{Kind: nitrado.KindPermission, StatusCode: 403}, "token does not have permission for this request"},
		{"not_found", &nitrado.RequestError{Kind: nitrado.KindNotFound, StatusCode: 404}, "not found"},
		{"rate_limited", &nitrado.RequestError{Kind: nitrado.KindTemporary, StatusCode: 429}, "rate limited by Nitrado; try again shortly"},
		{"temporary", &nitrado.RequestError{Kind: nitrado.KindTemporary, StatusCode: 500}, "Nitrado is temporarily unavailable"},
		{"unknown_kind", &nitrado.RequestError{Kind: nitrado.KindUnknown, StatusCode: 418}, "unexpected Nitrado response"},
		{"non_request_error", errors.New("dial tcp: connection refused"), "could not reach Nitrado"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyNitradoErr(tc.err)
			if got != tc.want {
				t.Fatalf("classifyNitradoErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestDisplayNameFromServicePrefersServerName(t *testing.T) {
	svc := nitrado.Service{Game: "DayZ (PS4)", Details: nitrado.ServiceDetails{ServerName: "My Server", Name: "fallback"}}
	if got := displayNameFromService(svc); got != "My Server" {
		t.Fatalf("expected server_name to win, got %q", got)
	}
	svc2 := nitrado.Service{Game: "DayZ (PS4)", Details: nitrado.ServiceDetails{Name: "fallback name"}}
	if got := displayNameFromService(svc2); got != "fallback name" {
		t.Fatalf("expected details.name fallback, got %q", got)
	}
	svc3 := nitrado.Service{Game: "DayZ (PS4)"}
	if got := displayNameFromService(svc3); got != "DayZ (PS4)" {
		t.Fatalf("expected game fallback, got %q", got)
	}
	svc4 := nitrado.Service{}
	if got := displayNameFromService(svc4); got != "DayZ Server" {
		t.Fatalf("expected generic fallback, got %q", got)
	}
}

func TestTruncateLabelRespectsDiscordLimit(t *testing.T) {
	short := "short label"
	if got := truncateLabel(short, 100); got != short {
		t.Fatalf("short string should be unchanged, got %q", got)
	}
	long := ""
	for i := 0; i < 200; i++ {
		long += "a"
	}
	got := truncateLabel(long, 100)
	if len(got) != 100 {
		t.Fatalf("expected truncation to exactly 100 chars, got %d", len(got))
	}
}
