// Command case-staff-preview prints the fixed synthetic C.A.S.E. embed as
// Discord-compatible JSON. It has no network client, credentials, or send path.
package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/yourname/dayz-killfeed/internal/discord"
)

func writePreview(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(discord.BuildCASEStaffDemoEmbed())
}

func main() {
	if err := writePreview(os.Stdout); err != nil {
		os.Exit(1)
	}
}
