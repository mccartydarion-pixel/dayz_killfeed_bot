package welcome

import (
	"fmt"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"strings"
	"time"
)

type Service struct{ repo *repository.WelcomeRepository }

func NewService(r *repository.WelcomeRepository) *Service { return &Service{repo: r} }
func Render(text string, vars map[string]string) (string, error) {
	allowed := map[string]bool{"user": true, "username": true, "server": true, "member_count": true, "link_command": true}
	for key, value := range vars {
		if !allowed[key] {
			return "", fmt.Errorf("unsupported welcome variable {%s}", key)
		}
		text = strings.ReplaceAll(text, "{"+key+"}", value)
	}
	return text, nil
}
func Defaults(guildID int64, channel string) repository.WelcomeConfig {
	return repository.WelcomeConfig{GuildID: guildID, Enabled: true, ChannelID: channel, MentionUser: true, ShowMemberCount: true, ShowServerName: true, ShowLinkInstructions: true}
}

var _ = time.Now
