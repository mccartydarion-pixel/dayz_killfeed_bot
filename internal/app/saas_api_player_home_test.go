package app

import (
	"encoding/json"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestPlayerHomeResponseWithoutServerIsExplicitNull(t *testing.T) {
	raw, err := json.Marshal(playerHomeResponse(repository.PlayerHome{InstallationID: 9}, false))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"server":null}` {
		t.Fatalf("got %s", raw)
	}
}

func TestPlayerHomeResponseCarriesEveryField(t *testing.T) {
	raw, err := json.Marshal(playerHomeResponse(repository.PlayerHome{
		InstallationID: 42, OrganizationID: 7, OrganizationName: "Org", ServerName: "DE #3", Platform: "XBOX",
		ServerStatus: "READY", DiscordGuildID: "123456789012345678", DiscordGuildName: "Guild", LinkStatus: "VERIFIED", Observed: true,
	}, true))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"server":{"installationId":42,"organizationId":7,"organizationName":"Org","serverName":"DE #3","platform":"XBOX","serverStatus":"READY","discordGuildId":"123456789012345678","discordGuildName":"Guild","linkStatus":"VERIFIED","observed":true}}`
	if string(raw) != want {
		t.Fatalf("got %s", raw)
	}
}
