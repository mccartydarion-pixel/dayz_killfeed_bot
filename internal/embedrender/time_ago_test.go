package embedrender

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

func TestTimeAgoReachesDiscordAsATimestamp(t *testing.T) {
	cfg := embedtemplates.Config{RouteKey: "KILLFEED", Enabled: true, Color: "#D4AF37",
		Title:       embedtemplates.Text{Enabled: true, Template: "PLAYER ELIMINATED"},
		Description: embedtemplates.Text{Enabled: true, Template: "{{time_ago}}\n{{killer}} killed {{victim}}"},
		Fields:      []embedtemplates.Field{}}
	render := func(timeAgo string) string {
		e, err := RenderEvent(cfg, "KILLFEED", map[string]string{"killer": "Alice", "victim": "Bob", "time_ago": timeAgo}, time.Unix(1791646433, 0))
		if err != nil {
			t.Fatal(err)
		}
		return e.Description
	}

	// Champion's own value is Discord timestamp syntax and must arrive unescaped.
	if got := render("<t:1791646433:R>"); !strings.HasPrefix(got, "<t:1791646433:R>\n") {
		t.Fatalf("description = %q, want the timestamp as written", got)
	}
	// Anything else under the name is sanitized like every other value: no mention, no markup.
	for _, hostile := range []string{"<@123456>", "<t:1:R> <@1>", "[x](https://evil.example)", "<t:1791646433:F>"} {
		got := render(hostile)
		if strings.Contains(got, "<@") || strings.HasPrefix(got, "[x](") || strings.HasPrefix(got, "<t:") {
			t.Errorf("time_ago %q rendered unsanitized: %q", hostile, got)
		}
	}
	// The designer's sample text is plain text and stays readable.
	if got := render("2 minutes ago"); !strings.HasPrefix(got, "2 minutes ago\n") {
		t.Fatalf("sample text = %q", got)
	}
}
