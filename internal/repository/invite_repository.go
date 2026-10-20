package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// InviteRepository stores Discord invite joins (internal/discord.InviteTracker) and reports which
// invites bring members who stay, link a game account and play.
type InviteRepository struct{ pool *pgxpool.Pool }

func NewInviteRepository(pool *pgxpool.Pool) *InviteRepository { return &InviteRepository{pool: pool} }

// InviteJoin is one member join and the invite it came through ("" code = unattributed).
type InviteJoin struct {
	GuildDiscordID   string
	MemberDiscordID  string
	Code             string
	InviterDiscordID string
	InviterName      string
	Attributed       bool
	JoinedAt         time.Time
}

// RecordInviteJoin stores a join for a guild Champion knows; joins in other guilds are ignored.
func (r *InviteRepository) RecordInviteJoin(ctx context.Context, j InviteJoin) error {
	if r == nil || r.pool == nil {
		return errors.New("invite repository unavailable")
	}
	_, err := r.pool.Exec(ctx, `
INSERT INTO discord_invite_joins(guild_id,member_discord_id,invite_code,inviter_discord_id,inviter_name,joined_at)
SELECT g.id,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6 FROM guilds g WHERE g.discord_guild_id=$1`,
		j.GuildDiscordID, j.MemberDiscordID, j.Code, j.InviterDiscordID, j.InviterName, j.JoinedAt)
	return err
}

// RecordInviteLeave stamps the member's most recent open join.
func (r *InviteRepository) RecordInviteLeave(ctx context.Context, guildDiscordID, memberDiscordID string, at time.Time) error {
	if r == nil || r.pool == nil {
		return errors.New("invite repository unavailable")
	}
	_, err := r.pool.Exec(ctx, `
UPDATE discord_invite_joins SET left_at=$3 WHERE id=(
  SELECT j.id FROM discord_invite_joins j JOIN guilds g ON g.id=j.guild_id
  WHERE g.discord_guild_id=$1 AND j.member_discord_id=$2 AND j.left_at IS NULL ORDER BY j.joined_at DESC LIMIT 1)`,
		guildDiscordID, memberDiscordID, at)
	return err
}

// InviteStat is one invite's (or the unattributed bucket's) results over the window.
type InviteStat struct {
	Code        string `json:"code"` // "" = joins Champion could not attribute
	InviterID   string `json:"inviterId,omitempty"`
	InviterName string `json:"inviterName,omitempty"`
	Joins       int64  `json:"joins"`
	StillIn     int64  `json:"stillIn"`
	Linked      int64  `json:"linked"`
	Played      int64  `json:"played"`
	ActiveNow   int64  `json:"activeNow"`
}

type InviteReport struct {
	Days    int          `json:"days"`
	Totals  InviteStat   `json:"totals"`
	Invites []InviteStat `json:"invites"`
}

// InviteReport groups joins in the last `days` by invite. Linked = verified game link for that
// Discord account; Played = the linked player was seen on any of the guild's servers after
// joining; ActiveNow = seen in the last 14 days.
func (r *InviteRepository) InviteReport(ctx context.Context, guildID int64, days int, now time.Time) (InviteReport, error) {
	out := InviteReport{Days: days, Invites: []InviteStat{}}
	rows, err := r.pool.Query(ctx, `
WITH j AS (
  SELECT DISTINCT ON (member_discord_id) member_discord_id, COALESCE(invite_code,'') AS code,
         COALESCE(inviter_discord_id,'') AS inviter_id, COALESCE(inviter_name,'') AS inviter_name, joined_at, left_at
  FROM discord_invite_joins WHERE guild_id=$1 AND joined_at >= $2::timestamptz - make_interval(days => $3)
  ORDER BY member_discord_id, joined_at DESC
), x AS (
  SELECT j.*, pl.player_id,
    EXISTS (SELECT 1 FROM player_daily_activity a WHERE a.guild_id=$1 AND a.player_id=pl.player_id AND a.last_seen_at >= j.joined_at) AS played,
    EXISTS (SELECT 1 FROM player_daily_activity a WHERE a.guild_id=$1 AND a.player_id=pl.player_id AND a.day >= ($2::timestamptz - INTERVAL '14 days')::date) AS active
  FROM j LEFT JOIN player_links pl ON pl.guild_id=$1 AND pl.discord_user_id=j.member_discord_id AND pl.status='VERIFIED'
)
SELECT code, MAX(inviter_id), MAX(inviter_name), COUNT(*), COUNT(*) FILTER (WHERE left_at IS NULL),
       COUNT(player_id), COUNT(*) FILTER (WHERE played), COUNT(*) FILTER (WHERE active)
FROM x GROUP BY code ORDER BY COUNT(*) FILTER (WHERE played) DESC, COUNT(*) DESC, code LIMIT 100`, guildID, now, days)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var s InviteStat
		if err := rows.Scan(&s.Code, &s.InviterID, &s.InviterName, &s.Joins, &s.StillIn, &s.Linked, &s.Played, &s.ActiveNow); err != nil {
			return out, err
		}
		out.Totals.Joins += s.Joins
		out.Totals.StillIn += s.StillIn
		out.Totals.Linked += s.Linked
		out.Totals.Played += s.Played
		out.Totals.ActiveNow += s.ActiveNow
		out.Invites = append(out.Invites, s)
	}
	return out, rows.Err()
}
