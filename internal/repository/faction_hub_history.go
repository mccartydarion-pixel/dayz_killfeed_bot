package repository

import (
	"context"
	"fmt"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// Membership history and public activity writes (Faction Hub Phase 5, docs/FACTION_STATS.md).
// Both are written INSIDE the transaction that changes the live membership, so history and
// membership can never disagree: a join opens a period, a leave/removal closes it, and the
// activity row exists exactly when the change committed.

// hubOpenPeriod records the start of a membership period for a member row that was just inserted.
// The period starts at the member's own joined_at, so the two are identical.
func hubOpenPeriod(ctx context.Context, q hubDB, memberID int64) error {
	_, err := q.Exec(ctx, `
INSERT INTO hub_faction_membership_history(organization_id, installation_id, faction_id, user_id, player_identity_id, joined_at)
SELECT f.organization_id, m.installation_id, m.faction_id, m.user_id, m.player_id, m.joined_at
FROM hub_faction_members m JOIN hub_factions f ON f.id = m.faction_id
WHERE m.id = $1`, memberID)
	if err != nil {
		return fmt.Errorf("hub open membership period: %w", err)
	}
	return nil
}

// hubClosePeriod ends the user's open membership period in the faction. Half-open: a kill at
// exactly left_at is NOT credited. It is a no-op when there is no open period (a member that
// predates the history table and was never backfilled - the migration backfills every member).
func hubClosePeriod(ctx context.Context, q hubDB, factionID, userID int64) error {
	_, err := q.Exec(ctx, `UPDATE hub_faction_membership_history SET left_at = clock_timestamp() WHERE faction_id=$1 AND user_id=$2 AND left_at IS NULL`, factionID, userID)
	if err != nil {
		return fmt.Errorf("hub close membership period: %w", err)
	}
	return nil
}

// hubActivity records one public-safe, non-combat faction activity event. detail is a role key
// or "" - never free text. subject is the member the event is about, actor who caused it (0 = none).
func hubActivity(ctx context.Context, q hubDB, factionID int64, eventType string, subjectUserID, actorUserID int64, detail string) error {
	_, err := q.Exec(ctx, `
INSERT INTO hub_faction_activity(organization_id, installation_id, faction_id, event_type, subject_user_id, actor_user_id, detail)
SELECT f.organization_id, f.installation_id, f.id, $2, NULLIF($3::bigint,0), NULLIF($4::bigint,0), NULLIF($5::text,'')
FROM hub_factions f WHERE f.id = $1`, factionID, eventType, subjectUserID, actorUserID, detail)
	if err != nil {
		return fmt.Errorf("hub record activity: %w", err)
	}
	return nil
}

// Activity event types the Hub itself records (combat events are derived at read time).
const (
	ActivityFactionCreated        = "FACTION_CREATED"
	ActivityMemberJoined          = "MEMBER_JOINED"
	ActivityMemberLeft            = "MEMBER_LEFT"
	ActivityMemberPromoted        = "MEMBER_PROMOTED"
	ActivityMemberDemoted         = "MEMBER_DEMOTED"
	ActivityLeadershipTransferred = "LEADERSHIP_TRANSFERRED"
	ActivityFactionUpdated        = "FACTION_UPDATED"
	ActivityFactionLogoChanged    = "FACTION_LOGO_CHANGED"
)

var _ = factionhub.RoleLeader // roles used as activity details are the factionhub role keys
