package discord

import (
	"encoding/json"
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// ApplicationSettings reads the bot's own application record (GET /applications/@me): whether
// the bot is public (customers can add it), whether installs require an OAuth2 code grant (which
// breaks the website's invite link), and the privacy policy and terms URLs the Developer Portal
// shows. The deploy self-check logs these so the portal's go-live settings are checked on every
// start, not remembered.
func ApplicationSettings(s *discordgo.Session) (*discordgo.Application, error) {
	if s == nil {
		return nil, fmt.Errorf("discord session is nil")
	}
	body, err := s.RequestWithBucketID("GET", discordgo.EndpointApplication("@me"), nil, discordgo.EndpointApplication(""))
	if err != nil {
		return nil, err
	}
	var app discordgo.Application
	if err := json.Unmarshal(body, &app); err != nil {
		return nil, fmt.Errorf("decode application: %w", err)
	}
	return &app, nil
}
