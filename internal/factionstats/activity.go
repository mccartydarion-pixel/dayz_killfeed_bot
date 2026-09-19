package factionstats

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Public activity event types. The keys are the contract with the website; do not rename.
const (
	ActivityFactionCreated        = "FACTION_CREATED"
	ActivityMemberJoined          = "MEMBER_JOINED"
	ActivityMemberLeft            = "MEMBER_LEFT"
	ActivityMemberPromoted        = "MEMBER_PROMOTED"
	ActivityMemberDemoted         = "MEMBER_DEMOTED"
	ActivityLeadershipTransferred = "LEADERSHIP_TRANSFERRED"
	ActivityFactionUpdated        = "FACTION_UPDATED"
	ActivityFactionLogoChanged    = "FACTION_LOGO_CHANGED"
	ActivityKill                  = "KILL"
	ActivityHeadshot              = "HEADSHOT"
	ActivityLongshot              = "LONGSHOT"
	ActivityBountyClaimed         = "BOUNTY_CLAIMED"
	ActivityServerRecord          = "SERVER_RECORD"
	ActivityAchievementUnlocked   = "ACHIEVEMENT_UNLOCKED"
)

// Pagination bounds (the project's list convention: a default page, a hard maximum, never unbounded).
const (
	DefaultActivityLimit = 20
	MaxActivityLimit     = 100
)

// MemberRef is the public identity shown on an event: display name, the Discord id/avatar the site
// already exposes on member lists, and the linked gamertag when there is a verified link. No
// internal numeric id.
type MemberRef struct {
	DiscordUserID string  `json:"discordUserId"`
	DisplayName   string  `json:"displayName"`
	Avatar        string  `json:"avatar,omitempty"`
	Gamertag      *string `json:"gamertag"`
}

// ActivityEvent is one public-safe event. It never carries application messages, moderation
// detail (a removal reads as MEMBER_LEFT), free text or internal ids: id is an opaque event key.
type ActivityEvent struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	OccurredAt string         `json:"occurredAt"`
	Member     *MemberRef     `json:"member"`
	Details    map[string]any `json:"details,omitempty"`
}

// ActivityPage is one keyset page, newest first.
type ActivityPage struct {
	Items      []ActivityEvent `json:"items"`
	NextCursor *string         `json:"nextCursor"`
	Limit      int             `json:"limit"`
}

var sourceLetter = map[int]string{
	repository.ActivitySrcHub: "h", repository.ActivitySrcKill: "k", repository.ActivitySrcBounty: "b",
	repository.ActivitySrcRecord: "r", repository.ActivitySrcUnlock: "u",
}

// GetFactionRecentActivity returns the faction's public activity, newest first, paginated. The
// limit is clamped to [1, MaxActivityLimit] (0 = DefaultActivityLimit); cursor is the previous
// page's nextCursor (nil = first page).
func (s *Service) GetFactionRecentActivity(ctx context.Context, organizationID, installationID, factionID int64, limit int, cursor *repository.HubActivityCursor) (*ActivityPage, error) {
	if limit <= 0 {
		limit = DefaultActivityLimit
	}
	if limit > MaxActivityLimit {
		limit = MaxActivityLimit
	}
	scope, err := s.store.Scope(ctx, organizationID, installationID, factionID)
	if err != nil {
		return nil, err
	}
	rows, more, err := s.store.Activity(ctx, scope, limit, cursor)
	if err != nil {
		return nil, err
	}
	page := &ActivityPage{Items: make([]ActivityEvent, 0, len(rows)), Limit: limit}
	for _, r := range rows {
		page.Items = append(page.Items, toEvent(r))
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		c := encodeCursor(last.At, last.Src, last.ID)
		page.NextCursor = &c
	}
	return page, nil
}

func toEvent(r repository.HubActivityRow) ActivityEvent {
	ev := ActivityEvent{ID: sourceLetter[r.Src] + itoa(r.ID), Type: r.Type, OccurredAt: r.At.UTC().Format(time.RFC3339)}
	if r.SubjectDiscordID != "" {
		ev.Member = &MemberRef{DiscordUserID: r.SubjectDiscordID, DisplayName: displayName(r.SubjectGlobalName, r.SubjectUsername), Avatar: r.SubjectAvatar, Gamertag: r.SubjectGamertag}
	}
	d := map[string]any{}
	switch r.Type {
	case ActivityMemberPromoted, ActivityMemberDemoted:
		if r.Detail != nil {
			d["role"] = *r.Detail
		}
	case ActivityLeadershipTransferred:
		if r.ActorUsername != nil {
			name := *r.ActorUsername
			if r.ActorGlobalName != nil && *r.ActorGlobalName != "" {
				name = *r.ActorGlobalName
			}
			d["previousLeader"] = name
		}
	case ActivityKill, ActivityHeadshot, ActivityLongshot:
		if r.VictimName != nil {
			d["victim"] = *r.VictimName
		}
		if r.Weapon != nil && *r.Weapon != "" {
			d["weapon"] = *r.Weapon
		}
		if r.Distance != nil {
			d["distanceMeters"] = math.Round(*r.Distance*10) / 10
		}
		d["headshot"], d["longshot"] = r.Headshot, r.Longshot
	case ActivityBountyClaimed:
		if r.VictimName != nil {
			d["target"] = *r.VictimName
		}
		d["rewardPoints"], d["bounties"] = r.RewardPoints, r.BountyCount
	case ActivityServerRecord:
		if r.RecordType != nil {
			d["record"] = *r.RecordType
		}
	case ActivityAchievementUnlocked:
		if r.AchievementID != nil {
			d["key"] = *r.AchievementID
			if def, ok := DefinitionByKey(*r.AchievementID); ok {
				d["name"] = def.Name
			}
		}
	}
	if len(d) > 0 {
		ev.Details = d
	}
	return ev
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
