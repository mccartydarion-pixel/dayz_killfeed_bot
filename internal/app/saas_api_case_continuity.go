package app

import (
 "context"
 "time"

 "github.com/jackc/pgx/v5/pgxpool"
)

// Source-group summaries describe retained observations, not all ADM lines.
// Each source identity stays scoped to an authorized installation.
type caseContinuitySource struct {
 SourceRef string `json:"sourceRef"`
 RecordedLines int64 `json:"recordedLines"`
 HitLines int64 `json:"hitLines"`
 KillLines int64 `json:"killLines"`
 RespawnLines int64 `json:"respawnLines"`
 ConnectLines int64 `json:"connectLines"`
 DisconnectLines int64 `json:"disconnectLines"`
 HighestObservedOffset int64 `json:"highestObservedOffset"`
 LatestIngestedAt time.Time `json:"latestIngestedAt"`
}

// This is an intentionally bounded read; source IDs, player identities and
// raw ADM lines are never returned. No source is joined to another boot.
func caseContinuitySources(ctx context.Context,pool *pgxpool.Pool,guildID,serverID int64,limit int) ([]caseContinuitySource,error) {
 if limit<1||limit>10 {limit=5}
 rows,err:=pool.Query(ctx,`
  SELECT source_id,COUNT(*),
    COUNT(*) FILTER (WHERE event_type='PLAYER_HIT'),
    COUNT(*) FILTER (WHERE event_type='PLAYER_KILL'),
    COUNT(*) FILTER (WHERE event_type='PLAYER_RESPAWN'),
    COUNT(*) FILTER (WHERE event_type='PLAYER_CONNECT'),
    COUNT(*) FILTER (WHERE event_type='PLAYER_DISCONNECT'),
    MAX(source_end_offset),MAX(ingested_at)
  FROM case_evidence_events
  WHERE guild_id=$1 AND server_id=$2
  GROUP BY source_id
  ORDER BY MAX(id) DESC
  LIMIT $3
 `,guildID,serverID,limit)
 if err!=nil{return nil,err}
 defer rows.Close()
 out:=make([]caseContinuitySource,0,limit)
 for rows.Next(){
  var source string
  var item caseContinuitySource
  if err=rows.Scan(&source,&item.RecordedLines,&item.HitLines,&item.KillLines,
   &item.RespawnLines,&item.ConnectLines,&item.DisconnectLines,
   &item.HighestObservedOffset,&item.LatestIngestedAt);err!=nil{return nil,err}
  item.SourceRef=*caseSourceRef(source)
  out=append(out,item)
 }
 return out,rows.Err()
}

type caseContinuityReport struct {
 CoverageStatus string `json:"coverageStatus"`
 SourceHistoryLimited bool `json:"sourceHistoryLimited"`
 RecentSources []caseContinuitySource `json:"recentSources"`
 CurrentSourceEvidenceStatus string `json:"currentSourceEvidenceStatus"`
 CurrentSourceRecordedLines int64 `json:"currentSourceRecordedLines"`
 CurrentSourceHitLines int64 `json:"currentSourceHitLines"`
 CurrentSourceKillLines int64 `json:"currentSourceKillLines"`
 CurrentSourceRespawnLines int64 `json:"currentSourceRespawnLines"`
 LatestCurrentSourceIngestedAt *time.Time `json:"latestCurrentSourceIngestedAt"`
 CheckpointContinuity string `json:"checkpointContinuity"`
 ReplayProtection string `json:"replayProtection"`
 RestartRecovery string `json:"restartRecovery"`
 TrustedElapsedTime bool `json:"trustedElapsedTime"`
 DetectorsEnabled bool `json:"detectorsEnabled"`
 Enforcement string `json:"enforcement"`
}

func caseContinuityAssessment(sourceRef *string,sources []caseContinuitySource,workerAvailable,collectorConfigured bool,checkpointKnown bool) caseContinuityReport {
 out:=caseContinuityReport{
  CoverageStatus:"SELECTED_EVENTS_ONLY_NOT_FULL_ADM_COVERAGE",
  SourceHistoryLimited:len(sources)==10,
  RecentSources:sources,
  CurrentSourceEvidenceStatus:"UNKNOWN",
  CheckpointContinuity:"NOT_PROVEN",
  ReplayProtection:"SOURCE_ADDRESS_AND_LINE_HASH",
  RestartRecovery:"NOT_TESTED_ON_LIVE_SERVER",
  TrustedElapsedTime:false,DetectorsEnabled:false,Enforcement:"DISABLED",
 }
 if !workerAvailable||!collectorConfigured||sourceRef==nil {return out}
 out.CurrentSourceEvidenceStatus="NO_RETAINED_EVENTS_IN_RETURNED_SOURCES"
 for _,s:=range sources {
  if s.SourceRef!=*sourceRef {continue}
  out.CurrentSourceEvidenceStatus="RETAINED_EVENTS_OBSERVED"
  out.CurrentSourceRecordedLines=s.RecordedLines
  out.CurrentSourceHitLines=s.HitLines
  out.CurrentSourceKillLines=s.KillLines
  out.CurrentSourceRespawnLines=s.RespawnLines
  when:=s.LatestIngestedAt
  out.LatestCurrentSourceIngestedAt=&when
  break
 }
 // Comparing offset with remote file length is *not* evidence of complete
 // collection: most ADM lines are intentionally not persisted by C.A.S.E.
 if checkpointKnown {out.CheckpointContinuity="CHECKPOINT_REPORTED_NOT_FULL_COVERAGE"}
 return out
}
