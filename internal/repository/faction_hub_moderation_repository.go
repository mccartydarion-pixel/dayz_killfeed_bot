package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// Server moderation of Faction Hub factions (docs/CLIENT_ADMIN.md, FACTION_MODERATE /
// FACTION_DISSOLVE). These bypass the faction-role matrix: the actor is a server staff member
// resolved by the client admin control plane, never a faction member. Every method is scoped to
// (organization, installation) like the player-facing ones, so a foreign faction is a not-found.

// HubModeratedFaction is a faction row plus the details a moderation list shows.
type HubModeratedFaction struct {
	HubFaction
	Leader           *HubMember
	PendingApplicant int
	LastActivityAt   *time.Time
}

// ListFactionsForModeration returns every faction on the installation (newest first) with its
// leader, pending application count and last activity, capped at limit.
func (r *FactionHubRepository) ListFactionsForModeration(ctx context.Context, organizationID, installationID int64, limit int) ([]HubModeratedFaction, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	if _, _, err := hubInstallation(ctx, r.pool, organizationID, installationID); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+hubFactionCols+`,
 (SELECT COUNT(*) FROM hub_faction_applications a WHERE a.faction_id = f.id AND a.status = 'PENDING'),
 (SELECT MAX(x.occurred_at) FROM hub_faction_activity x WHERE x.faction_id = f.id)
FROM `+hubFactionFrom+`
WHERE f.organization_id=$1 AND f.installation_id=$2
ORDER BY f.id DESC LIMIT $3`, organizationID, installationID, limit)
	if err != nil {
		return nil, fmt.Errorf("hub moderation list: %w", err)
	}
	defer rows.Close()
	var out []HubModeratedFaction
	for rows.Next() {
		var m HubModeratedFaction
		f, err := scanHubFactionWith(rows, &m.PendingApplicant, &m.LastActivityAt)
		if err != nil {
			return nil, fmt.Errorf("hub moderation scan: %w", err)
		}
		m.HubFaction = f
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	// One query for every listed faction's leader (one row per faction, the lowest member id
	// when a faction somehow has two) instead of one QueryRow per faction.
	ids := make([]int64, len(out))
	index := make(map[int64]int, len(out))
	for i := range out {
		ids[i] = out[i].ID
		index[out[i].ID] = i
	}
	lrows, err := r.pool.Query(ctx, `SELECT DISTINCT ON (m.faction_id) `+hubMemberCols+` `+hubMemberFrom+`
 WHERE m.faction_id = ANY($1) AND m.role_key='LEADER' ORDER BY m.faction_id, m.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("hub moderation leader: %w", err)
	}
	defer lrows.Close()
	for lrows.Next() {
		leader, err := scanHubMember(lrows)
		if err != nil {
			return nil, fmt.Errorf("hub moderation leader: %w", err)
		}
		if i, ok := index[leader.FactionID]; ok {
			l := leader
			out[i].Leader = &l
		}
	}
	if err := lrows.Err(); err != nil {
		return nil, fmt.Errorf("hub moderation leader: %w", err)
	}
	return out, nil
}

// ModerateFaction applies a profile update as server staff (no faction role required).
func (r *FactionHubRepository) ModerateFaction(ctx context.Context, organizationID, installationID, factionID, actorUserID int64, in HubFactionUpdate) (*HubFaction, error) {
	var out HubFaction
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f"); err != nil {
			return err
		}
		if err := hubApplyUpdate(ctx, tx, organizationID, installationID, factionID, actorUserID, in); err != nil {
			return err
		}
		var err error
		out, err = hubFactionFull(ctx, tx, organizationID, installationID, factionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ModerateRemoveMember removes any non-leader member as server staff. The leader is protected:
// transfer leadership first (ModerateTransferLeadership) or dissolve the faction.
func (r *FactionHubRepository) ModerateRemoveMember(ctx context.Context, organizationID, installationID, factionID, memberID, actorUserID int64) (*HubMember, error) {
	var out HubMember
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f"); err != nil {
			return err
		}
		target, err := hubMemberByID(ctx, tx, factionID, memberID, "FOR UPDATE OF m")
		if err != nil {
			return err
		}
		if target.RoleKey == factionhub.RoleLeader {
			return factionhub.ErrLeaderProtected
		}
		if _, err := tx.Exec(ctx, `DELETE FROM hub_faction_members WHERE id=$1`, memberID); err != nil {
			return fmt.Errorf("hub moderation remove member: %w", err)
		}
		if err := hubClosePeriod(ctx, tx, factionID, target.User.ID); err != nil {
			return err
		}
		// Public activity does not distinguish leaving from being removed (moderation stays private).
		if err := hubActivity(ctx, tx, factionID, ActivityMemberLeft, target.User.ID, 0, ""); err != nil {
			return err
		}
		out = target
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ModerateTransferLeadership makes targetMemberID the leader as server staff; the previous
// leader (if any) becomes an officer.
func (r *FactionHubRepository) ModerateTransferLeadership(ctx context.Context, organizationID, installationID, factionID, targetMemberID, actorUserID int64) (newLeader HubMember, err error) {
	err = r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f"); err != nil {
			return err
		}
		target, err := hubMemberByID(ctx, tx, factionID, targetMemberID, "FOR UPDATE OF m")
		if err != nil {
			return err
		}
		if target.RoleKey == factionhub.RoleLeader {
			return factionhub.ErrAlreadyLeader
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_members SET role_key='OFFICER' WHERE faction_id=$1 AND role_key='LEADER'`, factionID); err != nil {
			return fmt.Errorf("hub moderation transfer demote: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_members SET role_key='LEADER' WHERE id=$1 AND faction_id=$2`, target.ID, factionID); err != nil {
			return fmt.Errorf("hub moderation transfer promote: %w", err)
		}
		if err := hubActivity(ctx, tx, factionID, ActivityLeadershipTransferred, target.User.ID, actorUserID, ""); err != nil {
			return err
		}
		newLeader, err = hubMemberByID(ctx, tx, factionID, target.ID, "")
		return err
	})
	return newLeader, err
}

// DissolveFaction deletes the faction as server staff. Members, applications, roles, settings,
// history, activity and logo metadata go with it (schema cascades); logo bytes are reclaimed by
// the asset orphan sweeper. It returns the faction as it was, for the audit record.
func (r *FactionHubRepository) DissolveFaction(ctx context.Context, organizationID, installationID, factionID int64) (*HubFaction, error) {
	var out HubFaction
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		f, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f")
		if err != nil {
			return err
		}
		// Close every member's membership period so per-member stats history stays truthful.
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_membership_history SET left_at = NOW() WHERE faction_id=$1 AND left_at IS NULL`, factionID); err != nil {
			return fmt.Errorf("hub dissolve close periods: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM hub_factions WHERE id=$1 AND organization_id=$2 AND installation_id=$3`, factionID, organizationID, installationID); err != nil {
			return fmt.Errorf("hub dissolve: %w", err)
		}
		out = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
