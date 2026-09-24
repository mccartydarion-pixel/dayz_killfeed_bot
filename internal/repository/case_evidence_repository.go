package repository

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CaseEvidenceRepository is the source-addressed, strictly scoped observation
// sink for C.A.S.E. Phase 2B. It never writes to the existing killfeed tables.
// A record's key is its actual ADM source and byte offset, not the semantic
// fingerprint used for Discord dedupe (which can collapse repeated real hits).
type CaseEvidenceRepository struct{ pool *pgxpool.Pool }

func NewCaseEvidenceRepository(pool *pgxpool.Pool) *CaseEvidenceRepository {
	return &CaseEvidenceRepository{pool: pool}
}

type CaseEvidencePerson struct {
	DayZID string
	Name string
	X, Z, Altitude *float64
}

type CaseEvidenceInput struct {
	GuildID, ServerID int64
	SourceID string
	SourceEndOffset int64
	LineSHA256 string
	EventType string
	ADMClock string
	Subject, Actor, Target CaseEvidencePerson
	Weapon, Ammo, HitZone, HitZoneID string
	Damage, HP, DistanceMeters *float64
	BoundaryKind string
}

func caseBounded(s string, max int) string {
	if !utf8.ValidString(s) { return "" }
	r := []rune(s)
	if len(r) > max { return string(r[:max]) }
	return s
}

func caseEvidencePlayer(ctx context.Context, tx pgx.Tx, guildID int64, p CaseEvidencePerson) (*int64, error) {
	if p.DayZID == "" { return nil, nil }
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO players(guild_id,dayz_player_id,display_name,last_seen_at)
		VALUES($1,$2,$3,$4)
		ON CONFLICT(guild_id,dayz_player_id) DO UPDATE
		  SET last_seen_at=GREATEST(players.last_seen_at,EXCLUDED.last_seen_at)
		RETURNING id
	`, guildID, p.DayZID, caseBounded(p.Name, 128), time.Now().UTC()).Scan(&id)
	if err != nil { return nil, err }
	return &id, nil
}

// RecordCaseEvidence stores exactly one source line, or confirms a replay of
// the same line. A reused source offset with different content is a hard error:
// silently ignoring it would disguise truncation/replacement as a clean stream.
func (r *CaseEvidenceRepository) RecordCaseEvidence(ctx context.Context, item CaseEvidenceInput) error {
	if r == nil || r.pool == nil { return errors.New("C.A.S.E. repository unavailable") }
	if item.GuildID <= 0 || item.ServerID <= 0 || item.SourceID == "" || item.SourceEndOffset < 0 || len(item.LineSHA256) != 64 || item.EventType == "" {
		return errors.New("invalid C.A.S.E. source address")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil { return err }
	defer tx.Rollback(ctx)

	subjectID, err := caseEvidencePlayer(ctx, tx, item.GuildID, item.Subject)
	if err != nil { return fmt.Errorf("resolve subject: %w", err) }
	actorID, err := caseEvidencePlayer(ctx, tx, item.GuildID, item.Actor)
	if err != nil { return fmt.Errorf("resolve actor: %w", err) }
	targetID, err := caseEvidencePlayer(ctx, tx, item.GuildID, item.Target)
	if err != nil { return fmt.Errorf("resolve target: %w", err) }

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO case_evidence_events(
		  guild_id,server_id,source_id,source_end_offset,line_sha256,event_type,adm_clock,
		  subject_player_id,actor_player_id,target_player_id,subject_name,actor_name,target_name,
		  subject_x,subject_z,subject_altitude,actor_x,actor_z,actor_altitude,target_x,target_z,target_altitude,
		  weapon,ammo,hit_zone,hit_zone_id,damage,hp,distance_meters,boundary_kind)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)
		ON CONFLICT (guild_id,server_id,source_id,source_end_offset) DO NOTHING
		RETURNING id
	`,
		item.GuildID,item.ServerID,item.SourceID,item.SourceEndOffset,item.LineSHA256,item.EventType,caseBounded(item.ADMClock, 12),
		subjectID,actorID,targetID,caseBounded(item.Subject.Name,128),caseBounded(item.Actor.Name,128),caseBounded(item.Target.Name,128),
		item.Subject.X,item.Subject.Z,item.Subject.Altitude,item.Actor.X,item.Actor.Z,item.Actor.Altitude,item.Target.X,item.Target.Z,item.Target.Altitude,
		caseBounded(item.Weapon,128),caseBounded(item.Ammo,128),caseBounded(item.HitZone,64),caseBounded(item.HitZoneID,64),item.Damage,item.HP,item.DistanceMeters,item.BoundaryKind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var priorHash string
		err = tx.QueryRow(ctx, `SELECT line_sha256 FROM case_evidence_events
		  WHERE guild_id=$1 AND server_id=$2 AND source_id=$3 AND source_end_offset=$4`,
			item.GuildID,item.ServerID,item.SourceID,item.SourceEndOffset).Scan(&priorHash)
		if err != nil { return fmt.Errorf("verify replay: %w", err) }
		if priorHash != item.LineSHA256 { return errors.New("C.A.S.E. source offset collision: changed ADM line at prior position") }
		return nil
	}
	if err != nil { return fmt.Errorf("persist C.A.S.E. evidence: %w", err) }
	return tx.Commit(ctx)
}

type CaseEvidenceRow struct {
	ID int64 `json:"id"`
	Type string `json:"type"`
	IngestedAt time.Time `json:"ingestedAt"`
	ADMClock string `json:"admClock"`
	// SourceRef is a digest, not a raw Nitrado path or private identifier.
	SourceRef string `json:"sourceRef"`
	SourceEndOffset int64 `json:"sourceEndOffset"`
	SubjectPlayerID *int64 `json:"subjectPlayerId,omitempty"`
	ActorPlayerID *int64 `json:"actorPlayerId,omitempty"`
	TargetPlayerID *int64 `json:"targetPlayerId,omitempty"`
	SubjectName string `json:"subjectName,omitempty"`
	ActorName string `json:"actorName,omitempty"`
	TargetName string `json:"targetName,omitempty"`
	Weapon string `json:"weapon,omitempty"`
	Ammo string `json:"ammo,omitempty"`
	HitZone string `json:"hitZone,omitempty"`
	HitZoneID string `json:"hitZoneId,omitempty"`
	Damage *float64 `json:"damage,omitempty"`
	HP *float64 `json:"hp,omitempty"`
	DistanceMeters *float64 `json:"distanceMeters,omitempty"`
	BoundaryKind string `json:"boundaryKind,omitempty"`
	SubjectX *float64 `json:"subjectX,omitempty"`
	SubjectZ *float64 `json:"subjectZ,omitempty"`
	SubjectAltitude *float64 `json:"subjectAltitude,omitempty"`
	ActorX *float64 `json:"actorX,omitempty"`
	ActorZ *float64 `json:"actorZ,omitempty"`
	ActorAltitude *float64 `json:"actorAltitude,omitempty"`
	TargetX *float64 `json:"targetX,omitempty"`
	TargetZ *float64 `json:"targetZ,omitempty"`
	TargetAltitude *float64 `json:"targetAltitude,omitempty"`
}

// ListCaseEvidence requires an authorized guild AND server from AdminScope.
// The optional player ID selects only observations on this scoped server.
// Latest-first ID order reflects ingestion order, not an invented UTC ADM time.
func (r *CaseEvidenceRepository) ListCaseEvidence(ctx context.Context, guildID, serverID int64, playerID, beforeID *int64, limit int) ([]CaseEvidenceRow,error) {
	if limit < 1 || limit > 100 { limit=50 }
	rows,err:=r.pool.Query(ctx, `
	  SELECT id,event_type,ingested_at,adm_clock,
	         encode(sha256(convert_to(source_id,'UTF8')),'hex') AS source_ref,source_end_offset,
	         subject_player_id,actor_player_id,target_player_id,
	         subject_name,actor_name,target_name,weapon,ammo,hit_zone,hit_zone_id,
	         damage,hp,distance_meters,boundary_kind,
	         subject_x,subject_z,subject_altitude,actor_x,actor_z,actor_altitude,target_x,target_z,target_altitude
	  FROM case_evidence_events
	  WHERE guild_id=$1 AND server_id=$2
	    AND ($3::BIGINT IS NULL OR subject_player_id=$3 OR actor_player_id=$3 OR target_player_id=$3)
	    AND ($4::BIGINT IS NULL OR id<$4)
	  ORDER BY id DESC LIMIT $5
	`,guildID,serverID,playerID,beforeID,limit)
	if err!=nil {return nil,err}
	defer rows.Close()
	out:=make([]CaseEvidenceRow,0)
	for rows.Next(){
	  var e CaseEvidenceRow
	  if err=rows.Scan(&e.ID,&e.Type,&e.IngestedAt,&e.ADMClock,&e.SourceRef,&e.SourceEndOffset,
	    &e.SubjectPlayerID,&e.ActorPlayerID,&e.TargetPlayerID,
	    &e.SubjectName,&e.ActorName,&e.TargetName,&e.Weapon,&e.Ammo,&e.HitZone,&e.HitZoneID,
	    &e.Damage,&e.HP,&e.DistanceMeters,&e.BoundaryKind,
	    &e.SubjectX,&e.SubjectZ,&e.SubjectAltitude,&e.ActorX,&e.ActorZ,&e.ActorAltitude,&e.TargetX,&e.TargetZ,&e.TargetAltitude);err!=nil{return nil,err}
	  e.SourceRef=e.SourceRef[:16]
	  out=append(out,e)
	}
	return out,rows.Err()
}

func (r *CaseEvidenceRepository) CaseHitCount(ctx context.Context,guildID,serverID int64,from,to time.Time)(int64,error){
	var count int64
	err:=r.pool.QueryRow(ctx, `
	  SELECT COUNT(*) FROM case_evidence_events
	  WHERE guild_id=$1 AND server_id=$2 AND event_type='PLAYER_HIT' AND ingested_at >=$3 AND ingested_at <=$4
	`,guildID,serverID,from,to).Scan(&count)
	return count,err
}
