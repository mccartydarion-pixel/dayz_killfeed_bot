package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Client wraps the Discord session.
type Client struct {
	session *discordgo.Session
}

// New creates a Discord session using the minimally required intents for Phase 1.
func New(token string, membersIntent ...bool) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("discord token is required")
	}
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuilds
	if len(membersIntent) > 0 && membersIntent[0] {
		session.Identify.Intents |= discordgo.IntentsGuildMembers
	}
	return &Client{session: session}, nil
}

// Start connects the bot and registers the /server command.
//
// Open() returns as soon as the gateway handshake (Op 10 Hello) completes; it
// does NOT wait for the READY event that populates session.State.User. Command
// registration needs the bot's application ID from that user, so Start blocks
// (bounded) until READY arrives or a timeout is reached, to avoid the
// "discord application ID is not available" race during startup.
func (c *Client) Start(ctx context.Context) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("discord session not initialized")
	}
	if err := c.session.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	slog.Info("component=discord", "msg", "guild members intent requested in code", "requested", c.session.Identify.Intents&discordgo.IntentsGuildMembers != 0, "developer_portal_required", true)

	if !waitForSessionUser(ctx, c.session, 10*time.Second) {
		slog.Error("component=discord", "msg", "discord application ID is not available after connecting; command registration will fail", "err", "ready_event_timeout")
		return fmt.Errorf("discord application ID is not available: READY event did not arrive in time")
	}

	slog.Info("component=discord", "msg", "connected", "user", c.session.State.User.Username)
	return nil
}

// waitForSessionUser polls (bounded) for the READY event to populate
// session.State.User, which discordgo delivers asynchronously after Open()
// returns. Returns false if ctx is cancelled or timeout elapses first.
func waitForSessionUser(ctx context.Context, session *discordgo.Session, timeout time.Duration) bool {
	if session == nil {
		return false
	}
	if session.State != nil && session.State.User != nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if session.State != nil && session.State.User != nil {
				return true
			}
			if time.Now().After(deadline) {
				return session.State != nil && session.State.User != nil
			}
		}
	}
}

// GuildMembersIntentRequested reports whether the session requests the
// privileged intent required for GuildMemberAdd delivery.
func (c *Client) GuildMembersIntentRequested() bool {
	return c != nil && c.session != nil && c.session.Identify.Intents&discordgo.IntentsGuildMembers != 0
}

// BotUsername returns the connected bot's username, or an empty string.
func (c *Client) BotUsername() string {
	if c == nil || c.session == nil || c.session.State == nil || c.session.State.User == nil {
		return ""
	}
	return c.session.State.User.Username
}

// BotID returns the connected bot's user ID, or an empty string.
func (c *Client) BotID() string {
	if c == nil || c.session == nil || c.session.State == nil || c.session.State.User == nil {
		return ""
	}
	return c.session.State.User.ID
}

// AddHandler registers an event handler on the underlying session.
func (c *Client) AddHandler(fn func(*discordgo.Session, *discordgo.InteractionCreate)) {
	if c == nil || c.session == nil {
		return
	}
	c.session.AddHandler(fn)
}

// AddMemberJoinHandler registers a GuildMemberAdd listener.
func (c *Client) AddMemberJoinHandler(fn func(*discordgo.Session, *discordgo.GuildMemberAdd)) {
	if c == nil || c.session == nil {
		return
	}
	c.session.AddHandler(fn)
}

// Verification describes the result of guild/channel/permission validation.
type Verification struct {
	GuildFound   bool
	ChannelFound bool
	Missing      []string
}

// Verify checks that the configured guild and killfeed channel exist and that
// the bot holds the permissions required for future killfeed messages.
// Findings are logged clearly; nothing fails silently.
func (c *Client) Verify(guildID, channelID string) Verification {
	result := Verification{}
	if c == nil || c.session == nil {
		slog.Error("component=discord", "msg", "verification failed: session not initialized")
		return result
	}

	if guildID != "" {
		if _, err := c.session.Guild(guildID); err != nil {
			slog.Error("component=discord", "msg", "configured guild not found or not accessible", "guild_id", guildID)
		} else {
			result.GuildFound = true
			slog.Info("component=discord", "msg", "guild verified", "guild_id", guildID)
		}
	} else {
		slog.Warn("component=discord", "msg", "DISCORD_GUILD_ID not configured; skipping guild verification")
	}

	if channelID == "" {
		slog.Warn("component=discord", "msg", "KILLFEED_CHANNEL_ID not configured; skipping channel verification")
		return result
	}

	if _, err := c.session.Channel(channelID); err != nil {
		slog.Error("component=discord", "msg", "killfeed channel not found or not accessible", "channel_id", channelID)
		return result
	}
	result.ChannelFound = true
	slog.Info("component=discord", "msg", "killfeed channel verified", "channel_id", channelID)

	userID := ""
	if c.session.State != nil && c.session.State.User != nil {
		userID = c.session.State.User.ID
	}
	if userID == "" {
		slog.Warn("component=discord", "msg", "bot user unavailable; skipping permission validation", "channel_id", channelID)
		return result
	}

	perms, err := c.session.UserChannelPermissions(userID, channelID)
	if err != nil {
		slog.Error("component=discord", "msg", "could not resolve channel permissions", "channel_id", channelID)
		return result
	}

	required := []struct {
		name string
		bit  int64
	}{
		{"View Channel", discordgo.PermissionViewChannel},
		{"Send Messages", discordgo.PermissionSendMessages},
		{"Embed Links", discordgo.PermissionEmbedLinks},
		{"Read Message History", discordgo.PermissionReadMessageHistory},
	}
	for _, req := range required {
		if perms&req.bit == 0 {
			result.Missing = append(result.Missing, req.name)
		}
	}
	if len(result.Missing) > 0 {
		slog.Error("component=discord",
			"msg", "missing killfeed channel permissions",
			"channel_id", channelID,
			"missing", result.Missing,
			"fix", "grant the bot these permissions on the channel (or a role it has), then restart",
		)
	} else {
		slog.Info("component=discord", "msg", "killfeed channel permissions verified", "channel_id", channelID)
	}
	return result
}

// Close shuts down the Discord session.
func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}

// Session returns the wrapped Discord session for command registration or future extensions.
func (c *Client) Session() *discordgo.Session {
	if c == nil {
		return nil
	}
	return c.session
}

func ApplicationID(s *discordgo.Session) (string, error) {
	if s == nil || s.State == nil || s.State.User == nil || s.State.User.ID == "" {
		return "", fmt.Errorf("discord application ID is not available")
	}
	return s.State.User.ID, nil
}
