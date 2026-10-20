package repository

import (
	"context"
	"encoding/json"
	"time"
)

// OwnerEvent is a competitive event as the Client Hub sees it: its prizes,
// template, announcement state and current top three.
type OwnerEvent struct {
	ID                                     int64           `json:"id"`
	Type                                   string          `json:"type"`
	Name                                   string          `json:"name"`
	Description                            string          `json:"description"`
	Status                                 string          `json:"status"`
	StartsAt                               *time.Time      `json:"startsAt"`
	EndsAt                                 *time.Time      `json:"endsAt"`
	Config                                 json.RawMessage `json:"config"`
	TemplateKey                            string          `json:"templateKey,omitempty"`
	Announce                               bool            `json:"announce"`
	FirstPoints, SecondPoints, ThirdPoints int             `json:"-"`
	Prizes                                 [3]int          `json:"prizes"`
	CreatedBy                              string          `json:"createdBy,omitempty"`
	Leaders                                []EventLeader   `json:"leaders"`
}

// EventLeader is one of an event's current top three.
type EventLeader struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
	Kills int64   `json:"kills"`
}

// CreateOwnerEvent inserts an owner-built event with its prizes and announce flag.
func (r *EventRepository) CreateOwnerEvent(ctx context.Context, guildID int64, e CompetitiveEvent, templateKey string, announce bool, prizes [3]int, createdBy string) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `INSERT INTO competitive_events(guild_id,season_id,event_type,name,description,status,starts_at,ends_at,created_by_discord_user_id,config,
  template_key,announce,winner_points,second_place_points,third_place_points)
VALUES($1,(SELECT id FROM seasons WHERE guild_id=$1 AND status='ACTIVE' LIMIT 1),$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13,$14) RETURNING id`,
		guildID, e.Type, e.Name, e.Description, e.Status, e.StartsAt, e.EndsAt, createdBy, e.Config, templateKey, announce, prizes[0], prizes[1], prizes[2]).Scan(&id)
	return id, err
}

// ListOwnerEvents returns the guild's events, upcoming and active first, then the most recent.
func (r *EventRepository) ListOwnerEvents(ctx context.Context, guildID int64, limit int) ([]OwnerEvent, error) {
	rows, err := r.pool.Query(ctx, `
SELECT e.id,e.event_type,e.name,COALESCE(e.description,''),e.status,e.starts_at,e.ends_at,e.config,COALESCE(e.template_key,''),e.announce,
       e.winner_points,e.second_place_points,e.third_place_points,COALESCE(e.created_by_discord_user_id,'')
FROM competitive_events e
WHERE e.guild_id=$1 AND e.event_type<>'HOT_ZONE'
ORDER BY CASE e.status WHEN 'ACTIVE' THEN 0 WHEN 'SCHEDULED' THEN 1 WHEN 'DRAFT' THEN 2 ELSE 3 END,
         CASE WHEN e.status IN ('ACTIVE','SCHEDULED','DRAFT') THEN e.starts_at END ASC NULLS LAST, e.id DESC
LIMIT $2`, guildID, clampLimit(limit, 50, 200))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OwnerEvent{}
	for rows.Next() {
		var e OwnerEvent
		if err := rows.Scan(&e.ID, &e.Type, &e.Name, &e.Description, &e.Status, &e.StartsAt, &e.EndsAt, &e.Config, &e.TemplateKey, &e.Announce,
			&e.FirstPoints, &e.SecondPoints, &e.ThirdPoints, &e.CreatedBy); err != nil {
			return nil, err
		}
		e.Prizes = [3]int{e.FirstPoints, e.SecondPoints, e.ThirdPoints}
		e.Leaders = []EventLeader{}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The top three of every ACTIVE or ENDED event in one query (ranked per event) instead of
	// one query per event. Order within an event is the same: score, kills, score id.
	var scored []int64
	index := map[int64]int{}
	for i := range out {
		if out[i].Status != "ACTIVE" && out[i].Status != "ENDED" {
			continue
		}
		scored = append(scored, out[i].ID)
		index[out[i].ID] = i
	}
	if len(scored) == 0 {
		return out, nil
	}
	lrows, err := r.pool.Query(ctx, `
SELECT t.event_id, COALESCE(p.display_name, f.name, 'Unknown'), t.score, t.kills
FROM (
  SELECT s.id, s.event_id, s.player_id, s.faction_id, s.score, s.kills,
         ROW_NUMBER() OVER (PARTITION BY s.event_id ORDER BY s.score DESC, s.kills DESC, s.id) AS rn
  FROM event_scores s WHERE s.event_id = ANY($1)
) t
LEFT JOIN players p ON p.id=t.player_id LEFT JOIN factions f ON f.id=t.faction_id
WHERE t.rn <= 3
ORDER BY t.event_id, t.rn`, scored)
	if err != nil {
		return nil, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var eventID int64
		var l EventLeader
		if err := lrows.Scan(&eventID, &l.Name, &l.Score, &l.Kills); err != nil {
			return nil, err
		}
		if i, ok := index[eventID]; ok {
			out[i].Leaders = append(out[i].Leaders, l)
		}
	}
	return out, lrows.Err()
}

// EventAnnouncement is a pending "upcoming" or "started" card.
type EventAnnouncement struct {
	Event                                  CompetitiveEvent
	Kind                                   string // UPCOMING | STARTED
	FirstPoints, SecondPoints, ThirdPoints int
}

// PendingEventAnnouncements lists announce-enabled events whose card has not been posted:
// STARTED for active events, UPCOMING for scheduled ones.
func (r *EventRepository) PendingEventAnnouncements(ctx context.Context, guildID int64, limit int) ([]EventAnnouncement, error) {
	rows, err := r.pool.Query(ctx, `
SELECT id,guild_id,COALESCE(season_id,0),event_type,name,COALESCE(description,''),status,starts_at,ends_at,config,
       CASE WHEN status='ACTIVE' THEN 'STARTED' ELSE 'UPCOMING' END, winner_points,second_place_points,third_place_points
FROM competitive_events
WHERE guild_id=$1 AND announce AND (
  (status='ACTIVE' AND start_announced_at IS NULL) OR (status='SCHEDULED' AND schedule_announced_at IS NULL))
ORDER BY id LIMIT $2`, guildID, clampLimit(limit, 10, 50))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventAnnouncement
	for rows.Next() {
		var a EventAnnouncement
		e := &a.Event
		if err := rows.Scan(&e.ID, &e.GuildID, &e.SeasonID, &e.Type, &e.Name, &e.Description, &e.Status, &e.StartsAt, &e.EndsAt, &e.Config,
			&a.Kind, &a.FirstPoints, &a.SecondPoints, &a.ThirdPoints); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkEventAnnounced stamps the card as posted (UPCOMING also covers the event
// moving straight to ACTIVE before the scheduled card went out).
func (r *EventRepository) MarkEventAnnounced(ctx context.Context, guildID, eventID int64, kind string, at time.Time) error {
	col := "schedule_announced_at"
	if kind == "STARTED" {
		col = "start_announced_at"
	}
	_, err := r.pool.Exec(ctx, `UPDATE competitive_events SET `+col+`=COALESCE(`+col+`,$3) WHERE guild_id=$1 AND id=$2`, guildID, eventID, at)
	return err
}
