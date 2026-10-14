package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Self-serve organization erase (docs/SAAS_HTTP_API.md "Erase an organization").
//
// Unlike DeleteEmptyOrganization (the platform owner's delete of an organization that never
// connected a server), this removes an organization that has been in use, at its own OWNER's
// request. It follows the same foreign-key graph: the organization row is deleted last and the
// configuration below it goes with it through ON DELETE CASCADE.
//
// Removed: the organization's Nitrado credential(s), its installations and their configuration
// and records, its Discord guild connections, its memberships, its subscription row and its
// organization audit log. Its game servers are switched off and released (exactly what
// DeleteInstallation does). Gameplay data of a guild it connected (players, kills, deaths, links,
// stats - everything keyed by the guilds row) is deleted only when no other organization still
// references that guild and it is not the bot's own configured guild; otherwise it is kept.
//
// Kept: payment records (billing_transactions, billing_webhook_events) and the one-trial marker
// (trial_grants), with their organization reference cleared; every user account; anything that
// belongs to another organization.

var (
	// ErrEraseSubscriptionActive: the organization (or a C.A.S.E. add-on of it) still has a live
	// paid Stripe subscription. Cancel it first.
	ErrEraseSubscriptionActive = errors.New("organization has a live paid subscription")
	// ErrEraseBlocked: the organization holds records the database refuses to delete (immutable
	// C.A.S.E. review history, add-on billing records). Nothing was changed.
	ErrEraseBlocked = errors.New("organization holds records that cannot be erased automatically")
)

// OrganizationEraseResult is what an erase did.
type OrganizationEraseResult struct {
	OrganizationID int64
	// GameServerIDs are the servers that were switched off; the caller stops their workers.
	GameServerIDs []int64
	// GuildsErased is how many guilds had their gameplay data deleted; GuildsKept how many were
	// left because another organization or the bot's own guild still uses them.
	GuildsErased int
	GuildsKept   int
}

// organizationLivePaid reports whether the organization or any of its C.A.S.E. add-ons has a live
// paid subscription (SubscriptionIsLivePaid).
func organizationLivePaid(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, organizationID int64) (bool, error) {
	var status, providerSub string
	err := q.QueryRow(ctx, `SELECT status, COALESCE(provider_subscription_id,'') FROM subscriptions WHERE organization_id = $1`, organizationID).Scan(&status, &providerSub)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("erase: subscription: %w", err)
	}
	if err == nil && SubscriptionIsLivePaid(providerSub, status) {
		return true, nil
	}
	var addonLive bool
	if err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM case_addon_subscriptions
               WHERE organization_id = $1 AND COALESCE(provider_subscription_id,'') <> ''
                 AND status NOT IN ($2, $3))`, organizationID, SubscriptionCanceled, SubscriptionInactive).Scan(&addonLive); err != nil {
		return false, fmt.Errorf("erase: add-on subscriptions: %w", err)
	}
	return addonLive, nil
}

// OrganizationErasePrecheck reads, without changing anything, whether the organization exists,
// whether a live paid subscription blocks an erase, and which game servers it runs (so the caller
// can stop their workers before the erase transaction).
func (r *PlatformOwnerRepository) OrganizationErasePrecheck(ctx context.Context, organizationID int64) (exists, livePaid bool, gameServerIDs []int64, err error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, false, nil, fmt.Errorf("erase precheck: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE id = $1)`, organizationID).Scan(&exists); err != nil {
		return false, false, nil, fmt.Errorf("erase precheck: organization: %w", err)
	}
	if !exists {
		return false, false, nil, nil
	}
	if livePaid, err = organizationLivePaid(ctx, tx, organizationID); err != nil {
		return false, false, nil, err
	}
	gameServerIDs, err = organizationGameServerIDs(ctx, tx, organizationID)
	return true, livePaid, gameServerIDs, err
}

func organizationGameServerIDs(ctx context.Context, tx pgx.Tx, organizationID int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `
SELECT id FROM game_servers WHERE organization_id = $1
UNION
SELECT game_server_id FROM installations WHERE organization_id = $1 AND game_server_id IS NOT NULL
ORDER BY 1`, organizationID)
	if err != nil {
		return nil, fmt.Errorf("erase: game servers: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("erase: game servers: %w", err)
	}
	return ids, nil
}

// isEraseRefusal reports whether err is the database refusing a delete: a RESTRICT foreign key
// (23503) or an immutability trigger (P0001 raise_exception).
func isEraseRefusal(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "P0001")
}

// EraseOrganization permanently erases the organization in one transaction (see the top of this
// file for exactly what is removed and kept). protectedDiscordGuildID is DISCORD_GUILD_ID: that
// guild's gameplay data is never deleted. actorDiscordID is recorded in platform_audit_log.
//
// Returns nil, nil when the organization does not exist; ErrEraseSubscriptionActive while a live
// paid subscription remains (checked again here under lock); ErrEraseBlocked when the database
// refuses the delete. The caller stops the returned game servers' workers after the commit.
func (r *PlatformOwnerRepository) EraseOrganization(ctx context.Context, organizationID int64, protectedDiscordGuildID, actorDiscordID string) (*OrganizationEraseResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("erase: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the organization, its installations and its subscription, as planOrganizationDelete
	// does, so nothing attaches a server or turns a subscription live mid-erase.
	var found int64
	err = tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id = $1 FOR UPDATE`, organizationID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("erase: organization: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM installations WHERE organization_id = $1 ORDER BY id FOR UPDATE`, organizationID); err != nil {
		return nil, fmt.Errorf("erase: lock installations: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM subscriptions WHERE organization_id = $1 FOR UPDATE`, organizationID); err != nil {
		return nil, fmt.Errorf("erase: lock subscription: %w", err)
	}
	livePaid, err := organizationLivePaid(ctx, tx, organizationID)
	if err != nil {
		return nil, err
	}
	if livePaid {
		return nil, ErrEraseSubscriptionActive
	}

	res := &OrganizationEraseResult{OrganizationID: organizationID}
	if res.GameServerIDs, err = organizationGameServerIDs(ctx, tx, organizationID); err != nil {
		return nil, err
	}
	// The guilds this organization touched, collected before its connections are deleted.
	guildRows, err := tx.Query(ctx, `
SELECT guild_id FROM discord_guild_connections WHERE organization_id = $1
UNION
SELECT guild_id FROM game_servers WHERE id = ANY($2) AND guild_id IS NOT NULL
ORDER BY 1`, organizationID, res.GameServerIDs)
	if err != nil {
		return nil, fmt.Errorf("erase: guilds: %w", err)
	}
	guildIDs, err := pgx.CollectRows(guildRows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("erase: guilds: %w", err)
	}

	// 1. Nitrado credentials stored for the organization (the reference is SET NULL, so they would
	//    otherwise stay behind with no owner).
	if _, err := tx.Exec(ctx, `DELETE FROM nitrado_connections WHERE organization_id = $1`, organizationID); err != nil {
		return nil, fmt.Errorf("erase: nitrado credentials: %w", err)
	}
	// 2. Switch off and release its game servers (DeleteInstallation's rule), so no worker,
	//    scheduler or reconciler picks them up again.
	if len(res.GameServerIDs) > 0 {
		if _, err := tx.Exec(ctx, `
UPDATE game_servers SET active = FALSE, status = 'DISCONNECTED', organization_id = NULL, updated_at = NOW()
WHERE id = ANY($1)`, res.GameServerIDs); err != nil {
			return nil, fmt.Errorf("erase: switch off game servers: %w", err)
		}
	}
	// 3. Installations (and everything cascading below them), then the Discord guild connections,
	//    then the organization itself (memberships, subscription, organization audit log cascade;
	//    payment records and the trial marker are kept with organization_id cleared).
	for _, step := range []struct{ what, sql string }{
		{"installations", `DELETE FROM installations WHERE organization_id = $1`},
		{"discord guild connections", `DELETE FROM discord_guild_connections WHERE organization_id = $1`},
		{"organization", `DELETE FROM organizations WHERE id = $1`},
	} {
		if _, err := tx.Exec(ctx, step.sql, organizationID); err != nil {
			if isEraseRefusal(err) {
				return nil, ErrEraseBlocked
			}
			return nil, fmt.Errorf("erase: %s: %w", step.what, err)
		}
	}

	// 4. Gameplay data, guild by guild, only where nothing else still uses the guild. Each guild
	//    runs in its own savepoint: a guild the database refuses to delete (records it keeps) is
	//    simply kept, and the erase of the organization still goes ahead.
	for _, guildID := range guildIDs {
		var shared bool
		if err := tx.QueryRow(ctx, `
SELECT g.discord_guild_id = $2
    OR EXISTS (SELECT 1 FROM discord_guild_connections c WHERE c.guild_id = g.id)
    OR EXISTS (SELECT 1 FROM game_servers gs WHERE gs.guild_id = g.id AND gs.organization_id IS NOT NULL)
    OR EXISTS (SELECT 1 FROM nitrado_connections nc WHERE nc.guild_id = g.id AND nc.organization_id IS NOT NULL)
FROM guilds g WHERE g.id = $1`, guildID, protectedDiscordGuildID).Scan(&shared); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("erase: guild usage: %w", err)
		}
		if shared {
			res.GuildsKept++
			continue
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("erase: savepoint: %w", err)
		}
		// ranked_seasons RESTRICTs its game server; it is gameplay data of this guild's servers.
		_, err = sp.Exec(ctx, `DELETE FROM ranked_seasons WHERE server_id IN (SELECT id FROM game_servers WHERE guild_id = $1)`, guildID)
		if err == nil {
			_, err = sp.Exec(ctx, `DELETE FROM guilds WHERE id = $1`, guildID)
		}
		if err != nil {
			_ = sp.Rollback(ctx)
			if !isEraseRefusal(err) {
				return nil, fmt.Errorf("erase: guild gameplay data: %w", err)
			}
			res.GuildsKept++
			continue
		}
		if err := sp.Commit(ctx); err != nil {
			return nil, fmt.Errorf("erase: release savepoint: %w", err)
		}
		res.GuildsErased++
	}

	if err := recordDeleteAudit(ctx, tx, actorDiscordID, "organization.erased", "organization", organizationID, organizationID,
		"self-serve erase by the organization owner", map[string]any{"organizationId": organizationID,
			"gameServers": len(res.GameServerIDs), "guildsErased": res.GuildsErased, "guildsKept": res.GuildsKept}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("erase: commit: %w", err)
	}
	return res, nil
}
