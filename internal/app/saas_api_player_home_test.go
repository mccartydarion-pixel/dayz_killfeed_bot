package app

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func homeJSON(t *testing.T, homes []repository.PlayerHome, preferred int64) string {
	t.Helper()
	raw, err := json.Marshal(playerHomeResponse(homes, preferred))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPlayerHomeResponseWithoutServerIsExplicitNull(t *testing.T) {
	for _, preferred := range []int64{0, 9} {
		if got := homeJSON(t, nil, preferred); got != `{"server":null,"servers":[],"selected":null}` {
			t.Fatalf("preferred=%d got %s", preferred, got)
		}
	}
}

func TestPlayerHomeResponseCarriesEveryField(t *testing.T) {
	got := homeJSON(t, []repository.PlayerHome{{
		InstallationID: 42, OrganizationID: 7, OrganizationName: "Org", ServerName: "DE #3", Platform: "XBOX",
		ServerStatus: "READY", DiscordGuildID: "123456789012345678", DiscordGuildName: "Guild", LinkStatus: "VERIFIED", Observed: true,
	}}, 0)
	server := `{"installationId":42,"organizationId":7,"organizationName":"Org","serverName":"DE #3","platform":"XBOX","serverStatus":"READY","discordGuildId":"123456789012345678","discordGuildName":"Guild","linkStatus":"VERIFIED","observed":true}`
	if want := `{"server":` + server + `,"servers":[` + server + `],"selected":"DEFAULT"}`; got != want {
		t.Fatalf("got %s", got)
	}
}

func TestPlayerHomeResponseSelection(t *testing.T) {
	homes := []repository.PlayerHome{{InstallationID: 5}, {InstallationID: 3}, {InstallationID: 8}}
	cases := []struct {
		name      string
		homes     []repository.PlayerHome
		preferred int64
		server    int64
		selected  string
	}{
		{"no preference", homes, 0, 5, "DEFAULT"},
		{"preference honoured", homes, 8, 8, "PREFERRED"},
		{"preference is the default", homes, 5, 5, "PREFERRED"},
		{"foreign installation ignored", homes, 99, 5, "DEFAULT"},
		{"negative ignored", homes, -3, 5, "DEFAULT"},
		{"single server", homes[:1], 3, 5, "DEFAULT"},
	}
	for _, c := range cases {
		resp := playerHomeResponse(c.homes, c.preferred)
		if resp.Server == nil || resp.Selected == nil || resp.Server.InstallationID != c.server || *resp.Selected != c.selected {
			t.Fatalf("%s: %+v", c.name, resp)
		}
		if len(resp.Servers) != len(c.homes) {
			t.Fatalf("%s: servers %+v", c.name, resp.Servers)
		}
		for i, home := range c.homes { // the list keeps the default order whatever is selected
			if resp.Servers[i].InstallationID != home.InstallationID {
				t.Fatalf("%s: order %+v", c.name, resp.Servers)
			}
		}
	}
}

func TestPlayerHomePreferenceParsing(t *testing.T) {
	for query, want := range map[string]int64{
		"": 0, "installationId=": 0, "installationId=42": 42, "installationId=abc": 0, "installationId=0": 0,
		"installationId=-4": 0, "installationId=4.5": 0, "installationId=99999999999999999999": 0, "installationId=7&installationId=9": 7,
	} {
		r := httptest.NewRequest("GET", "/api/saas/player/home?"+query, nil)
		if got := playerHomePreference(r); got != want {
			t.Fatalf("%q: got %d want %d", query, got, want)
		}
	}
}
