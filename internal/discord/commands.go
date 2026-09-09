package discord

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// RegisterServerCommand registers the /server slash command.
func RegisterServerCommand(session *discordgo.Session, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("discord session is nil")
	}

	cmd := &discordgo.ApplicationCommand{
		Name:        "server",
		Description: "Display DayZ server status",
		Options:     []*discordgo.ApplicationCommandOption{},
	}

	_, err = session.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}
