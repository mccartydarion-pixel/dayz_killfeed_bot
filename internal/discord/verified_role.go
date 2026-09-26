package discord

import (
	"context"
	"fmt"
)

// VerifiedRoleAssigner adds the guild's configured @Verified role to a member
// once their /link request completes (either the ADM disconnect/reconnect
// challenge or an admin's manual approval). Unlike auto-created channels, the
// role is admin-supplied (/setup verified-role) rather than bot-created:
// a bot-created role would need correct hierarchy position to actually be
// assignable, which is a common failure mode best left to an admin who
// controls role order.
type VerifiedRoleAssigner struct {
	client  *Client
	store   SetupStore
	guildID string // Discord guild ID; this app serves a single guild per process
}

// NewVerifiedRoleAssigner creates an assigner bound to the guild setup store.
func NewVerifiedRoleAssigner(client *Client, store SetupStore, guildID string) *VerifiedRoleAssigner {
	return &VerifiedRoleAssigner{client: client, store: store, guildID: guildID}
}

// AssignVerifiedRole adds the configured verified role to discordUserID. If no
// role is configured, this returns a clear error but callers must treat that
// as non-fatal - a guild without a role set up still gets a verified link.
func (a *VerifiedRoleAssigner) AssignVerifiedRole(ctx context.Context, discordUserID string) error {
	if a == nil || a.client == nil || a.client.Session() == nil {
		return fmt.Errorf("discord client not ready")
	}
	if a.store == nil || a.guildID == "" {
		return fmt.Errorf("verified role store not configured")
	}
	setup, err := a.store.Get(a.guildID)
	if err != nil {
		return fmt.Errorf("load guild setup: %w", err)
	}
	if setup == nil || setup.VerifiedRoleID == "" {
		return fmt.Errorf("verified role not configured; run /setup verified-role")
	}
	if err := deliver("VERIFIED_ROLE", "", func() error {
		return a.client.Session().GuildMemberRoleAdd(a.guildID, discordUserID, setup.VerifiedRoleID)
	}); err != nil {
		return fmt.Errorf("assign verified role: %w", err)
	}
	return nil
}

// NotifyVerified DMs the player that their link finished verifying. Link
// completion can happen from background ADM log processing (the disconnect/
// reconnect challenge) with no active Discord interaction to reply to, so a
// DM is the only way to reach the player at that point.
func (a *VerifiedRoleAssigner) NotifyVerified(ctx context.Context, discordUserID string, roleAssigned bool) error {
	if a == nil || a.client == nil || a.client.Session() == nil {
		return fmt.Errorf("discord client not ready")
	}
	channel, err := a.client.Session().UserChannelCreate(discordUserID)
	if err != nil {
		return fmt.Errorf("open DM channel: %w", err)
	}
	message := "✅ **VERIFICATION COMPLETE**\n\nYour PlayStation account is now verified with Champion."
	if roleAssigned {
		message += " Your Verified role has been assigned."
	}
	if err := deliver("VERIFICATION_DM", "", func() error {
		_, err := a.client.Session().ChannelMessageSend(channel.ID, message)
		return err
	}); err != nil {
		return fmt.Errorf("send verification DM: %w", err)
	}
	return nil
}
