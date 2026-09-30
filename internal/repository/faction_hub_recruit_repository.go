package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// Faction recruitment (docs/FACTIONS.md "Recruitment"): instant joins for OPEN factions, and the
// one recruitment card a faction may keep in the FACTION_RECRUITMENT channel.

// HubRecruitPost is a faction's recruitment card in Discord.
type HubRecruitPost struct {
	FactionID, InstallationID int64
	ChannelID, MessageID      string
	PostedByUserID            *int64
	PostedAt, UpdatedAt       time.Time
}

// JoinFaction makes userID a MEMBER of an OPEN faction immediately (no application). The user
// must be in no faction on the installation; their pending applications elsewhere are cancelled,
// exactly as an accepted application would.
func (r *FactionHubRepository) JoinFaction(ctx context.Context, organizationID, installationID, factionID, userID int64) (*HubMember, error) {
	var out HubMember
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubWritableInstallation(ctx, tx, organizationID, installationID); err != nil {
			return err
		}
		if err := lockHubUser(ctx, tx, installationID, userID); err != nil {
			return err
		}
		f, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR SHARE OF f")
		if err != nil {
			return err
		}
		switch f.RecruitmentStatus {
		case factionhub.RecruitmentOpen:
		case factionhub.RecruitmentInviteOnly:
			return factionhub.ErrJoinRequiresOpen
		default:
			return factionhub.ErrRecruitmentClosed
		}
		if already, err := hubInstallationMembership(ctx, tx, installationID, userID); err != nil {
			return err
		} else if already {
			return factionhub.ErrAlreadyInFaction
		}
		memberID, err := hubAddMember(ctx, tx, factionID, installationID, userID, factionhub.RoleMember)
		if err != nil {
			return err
		}
		if err := hubCancelPending(ctx, tx, installationID, userID, 0); err != nil {
			return err
		}
		out, err = hubMemberByID(ctx, tx, factionID, memberID, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FactionForGuild loads a faction by id when its installation is connected to the given Discord
// guild (guilds.id), so a button pressed in that guild can only ever reach that guild's factions.
// It returns the organization and installation the faction belongs to.
func (r *FactionHubRepository) FactionForGuild(ctx context.Context, guildRowID, factionID int64) (*HubFaction, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+hubFactionCols+` FROM `+hubFactionFrom+`
JOIN installations i ON i.id = f.installation_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
WHERE f.id=$1 AND c.guild_id=$2`, factionID, guildRowID)
	f, err := scanHubFaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, factionhub.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hub faction for guild: %w", err)
	}
	return &f, nil
}

// RecruitPost returns the faction's recruitment card, or nil when it has none.
func (r *FactionHubRepository) RecruitPost(ctx context.Context, installationID, factionID int64) (*HubRecruitPost, error) {
	var p HubRecruitPost
	err := r.pool.QueryRow(ctx, `SELECT faction_id, installation_id, channel_id, message_id, posted_by_user_id, posted_at, updated_at
FROM hub_faction_recruit_posts WHERE faction_id=$1 AND installation_id=$2`, factionID, installationID).
		Scan(&p.FactionID, &p.InstallationID, &p.ChannelID, &p.MessageID, &p.PostedByUserID, &p.PostedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hub recruit post: %w", err)
	}
	return &p, nil
}

// UpsertRecruitPost records where the faction's card lives (a re-post replaces the record).
func (r *FactionHubRepository) UpsertRecruitPost(ctx context.Context, installationID, factionID int64, channelID, messageID string, postedBy *int64) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO hub_faction_recruit_posts(faction_id, installation_id, channel_id, message_id, posted_by_user_id)
VALUES($1,$2,$3,$4,$5)
ON CONFLICT (faction_id) DO UPDATE SET channel_id=EXCLUDED.channel_id, message_id=EXCLUDED.message_id,
  posted_by_user_id=COALESCE(EXCLUDED.posted_by_user_id, hub_faction_recruit_posts.posted_by_user_id), updated_at=NOW()`,
		factionID, installationID, channelID, messageID, postedBy)
	if err != nil {
		return fmt.Errorf("hub recruit post upsert: %w", err)
	}
	return nil
}

// TouchRecruitPost bumps updated_at after the card was edited in place.
func (r *FactionHubRepository) TouchRecruitPost(ctx context.Context, factionID int64) error {
	if _, err := r.pool.Exec(ctx, `UPDATE hub_faction_recruit_posts SET updated_at=NOW() WHERE faction_id=$1`, factionID); err != nil {
		return fmt.Errorf("hub recruit post touch: %w", err)
	}
	return nil
}

// DeleteRecruitPost forgets the card (the caller deletes the Discord message).
func (r *FactionHubRepository) DeleteRecruitPost(ctx context.Context, factionID int64) error {
	if _, err := r.pool.Exec(ctx, `DELETE FROM hub_faction_recruit_posts WHERE faction_id=$1`, factionID); err != nil {
		return fmt.Errorf("hub recruit post delete: %w", err)
	}
	return nil
}

// LeaderOf returns the faction's leader, or nil when it has none.
func (r *FactionHubRepository) LeaderOf(ctx context.Context, factionID int64) (*HubMember, error) {
	m, err := scanHubMember(r.pool.QueryRow(ctx, `SELECT `+hubMemberCols+` `+hubMemberFrom+` WHERE m.faction_id=$1 AND m.role_key='LEADER' LIMIT 1`, factionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hub leader: %w", err)
	}
	return &m, nil
}
