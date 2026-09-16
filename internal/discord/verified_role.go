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
	if err := a.client.Session().GuildMemberRoleAdd(a.guildID, discordUserID, setup.VerifiedRoleID); err != nil {
		return fmt.Errorf("assign verified role: %w", err)
	}
	return nil
}
