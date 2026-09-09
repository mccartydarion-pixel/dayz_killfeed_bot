package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/factions"
)

type Faction struct {
	ID, GuildID, OwnerPlayerID int64
	Name, Tag, DiscordRoleID   string
	Active                     bool
}
type FactionMember struct {
	FactionID, PlayerID int64
	Role                string
	Active              bool
	JoinedAt            time.Time
}
type FactionInvite struct {
	ID, FactionID, PlayerID int64
	Status                  string
	ExpiresAt               *time.Time
}

type FactionRepository struct{ pool *pgxpool.Pool }

func NewFactionRepository(pool *pgxpool.Pool) *FactionRepository {
	return &FactionRepository{pool: pool}
}

func (r *FactionRepository) CreateFaction(ctx context.Context, guildID, ownerPlayerID int64, name, tag string) (*Faction, error) {
	if err := factions.ValidateNameTag(name, tag); err != nil {
		return nil, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var f Faction
	err = tx.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,$2,$3,$4) RETURNING id,guild_id,name,tag,owner_player_id,active`, guildID, name, tag, ownerPlayerID).Scan(&f.ID, &f.GuildID, &f.Name, &f.Tag, &f.OwnerPlayerID, &f.Active)
	if err != nil {
		return nil, fmt.Errorf("create faction: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,$4)`, guildID, f.ID, ownerPlayerID, factions.RoleOwner); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO faction_membership_history(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,$4)`, guildID, f.ID, ownerPlayerID, factions.RoleOwner); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *FactionRepository) GetActiveFactionForPlayer(ctx context.Context, guildID, playerID int64) (*FactionMember, error) {
	var m FactionMember
	err := r.pool.QueryRow(ctx, `SELECT faction_id,player_id,role,active,joined_at FROM faction_members WHERE guild_id=$1 AND player_id=$2 AND active`, guildID, playerID).Scan(&m.FactionID, &m.PlayerID, &m.Role, &m.Active, &m.JoinedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *FactionRepository) GetMembership(ctx context.Context, guildID, playerID int64) (*FactionMember, error) {
	return r.GetActiveFactionForPlayer(ctx, guildID, playerID)
}

func (r *FactionRepository) GetByTag(ctx context.Context, guildID int64, tag string) (*Faction, error) {
	var f Faction
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,name,tag,owner_player_id,COALESCE(discord_role_id,''),active FROM factions WHERE guild_id=$1 AND LOWER(tag)=LOWER($2)`, guildID, tag).Scan(&f.ID, &f.GuildID, &f.Name, &f.Tag, &f.OwnerPlayerID, &f.DiscordRoleID, &f.Active)
	return &f, err
}

func (r *FactionRepository) Invite(ctx context.Context, guildID, factionID, playerID, invitedBy int64, expires time.Time) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO faction_invites(guild_id,faction_id,player_id,invited_by_player_id,status,expires_at) VALUES($1,$2,$3,$4,'PENDING',$5)`, guildID, factionID, playerID, invitedBy, expires)
	return err
}

func (r *FactionRepository) AcceptInvite(ctx context.Context, guildID, inviteID, playerID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var factionID int64
	var expires *time.Time
	if err := tx.QueryRow(ctx, `SELECT faction_id,expires_at FROM faction_invites WHERE id=$1 AND guild_id=$2 AND player_id=$3 AND status='PENDING' FOR UPDATE`, inviteID, guildID, playerID).Scan(&factionID, &expires); err != nil {
		return err
	}
	if expires != nil && time.Now().After(*expires) {
		return fmt.Errorf("invite expired")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,$4)`, guildID, factionID, playerID, factions.RoleMember); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO faction_membership_history(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,$4)`, guildID, factionID, playerID, factions.RoleMember); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE faction_invites SET status='ACCEPTED' WHERE id=$1`, inviteID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *FactionRepository) Leave(ctx context.Context, guildID, playerID int64) error {
	var role string
	if err := r.pool.QueryRow(ctx, `SELECT role FROM faction_members WHERE guild_id=$1 AND player_id=$2 AND active`, guildID, playerID).Scan(&role); err != nil {
		return err
	}
	if role == factions.RoleOwner {
		return factions.ErrOwnerCannotLeave
	}
	_, err := r.pool.Exec(ctx, `UPDATE faction_members SET active=false WHERE guild_id=$1 AND player_id=$2 AND active`, guildID, playerID)
	return err
}

func (r *FactionRepository) Disband(ctx context.Context, guildID, factionID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE factions SET active=false,updated_at=NOW() WHERE guild_id=$1 AND id=$2`, guildID, factionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE faction_members SET active=false WHERE guild_id=$1 AND faction_id=$2 AND active`, guildID, factionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
