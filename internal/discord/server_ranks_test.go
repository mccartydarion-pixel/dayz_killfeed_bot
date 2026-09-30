package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestServerRanksEmbedIsLocalAndBounded(t *testing.T) {
	rows := make([]repository.ServerStanding, 20)
	for i := range rows {
		rows[i] = repository.ServerStanding{Name: "Player", RP: int64(2000 - i*100), Tier: ranked.Gold}
	}
	rows[0].Name = "@everyone **winner**"
	embed := BuildServerRanksEmbed(ServerRanksSnapshot{
		ServerName: "Champions", SeasonName: "Season 1", UpdatedAt: time.Unix(1790683200, 0), Standings: rows,
	})
	if len(embed.Fields) != 15 {
		t.Fatalf("fields=%d, want 15", len(embed.Fields))
	}
	for _, field := range embed.Fields {
		if !field.Inline || !strings.Contains(field.Value, "RP") || !strings.Contains(field.Value, "GOLD") {
			t.Fatalf("bad ranked field: %+v", field)
		}
	}
	if strings.Contains(embed.Fields[0].Name, "@everyone") || !strings.Contains(embed.Description, "Season 1") || !strings.Contains(embed.Description, "<t:1790683200:R>") {
		t.Fatalf("unsafe or missing server rank context: %+v", embed)
	}
}

func TestServerRanksEmptySeason(t *testing.T) {
	embed := BuildServerRanksEmbed(ServerRanksSnapshot{ServerName: "Champions"})
	if len(embed.Fields) != 0 || !strings.Contains(embed.Description, "No qualifying players") {
		t.Fatalf("empty server ranks: %+v", embed)
	}
}
