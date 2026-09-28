package repository

import (
 "context"
 "errors"
 "fmt"
 "time"
)

// ShadowRunInput is an internal, explicit-call-only diagnostic operation.
// No ticker, startup hook, HTTP mutation, or collector invokes it.
type ShadowRunInput struct {
 Enabled bool
 GuildID,ServerID int64
 DetectorID,Version string
 Limit int
}
type ShadowRunResult struct {
 EvaluationID int64 `json:"evaluationId"`
 EvidenceCount int `json:"evidenceCount"`
 Status string `json:"status"`
}

// RunBlockedOnLatestSource selects a bounded snapshot of actual, already
// persisted evidence from exactly one server and one ADM source. It records
// only failed prerequisite diagnostics, never a cheat finding or player verdict.
// Every call rechecks the hard gate; zero-event sources cannot be evaluated.
func (r *ShadowLedger) RunBlockedOnLatestSource(ctx context.Context,in ShadowRunInput)(ShadowRunResult,error){
 if !in.Enabled{return ShadowRunResult{},ErrShadowDisabled}
 if r==nil||r.pool==nil{return ShadowRunResult{},errors.New("C.A.S.E. shadow database unavailable")}
 if in.GuildID<=0||in.ServerID<=0||in.Limit<1||in.Limit>50 {
  return ShadowRunResult{},errors.New("invalid shadow run scope or bound")
 }
 // Choose the source of the most recently persisted event. Source offset
 // orders events inside that source; ingestion IDs never imply gameplay time.
 rows,err:=r.pool.Query(ctx,`WITH latest_source AS (
    SELECT source_id FROM case_evidence_events WHERE guild_id=$1 AND server_id=$2
    ORDER BY id DESC LIMIT 1
   )
   SELECT id FROM case_evidence_events
   WHERE guild_id=$1 AND server_id=$2 AND source_id=(SELECT source_id FROM latest_source)
   ORDER BY source_end_offset DESC,id DESC LIMIT $3`,
   in.GuildID,in.ServerID,in.Limit)
 if err!=nil{return ShadowRunResult{},fmt.Errorf("read bounded shadow input: %w",err)}
 ids:=make([]int64,0,in.Limit)
 for rows.Next(){var id int64;if err=rows.Scan(&id);err!=nil{rows.Close();return ShadowRunResult{},err};ids=append(ids,id)}
 err=rows.Err();rows.Close();if err!=nil{return ShadowRunResult{},err}
 if len(ids)==0{return ShadowRunResult{},errors.New("no persisted source evidence")}
 id,err:=r.RecordBlocked(ctx,BlockedShadowInput{
  Enabled:in.Enabled,GuildID:in.GuildID,ServerID:in.ServerID,
  DetectorID:in.DetectorID,Version:in.Version,EvidenceIDs:ids,
 })
 if err!=nil{return ShadowRunResult{},err}
 return ShadowRunResult{EvaluationID:id,EvidenceCount:len(ids),Status:"BLOCKED"},nil
}

type ShadowHistoryRow struct {
 ID int64 `json:"id"`
 DetectorID string `json:"detectorId"`
 Version string `json:"version"`
 Status string `json:"status"`
 ReasonCodes []string `json:"reasonCodes"`
 EvidenceIDs []int64 `json:"evidenceIds"`
 CreatedAt time.Time `json:"createdAt"`
}

// ListShadowHistory is strictly tenant-and-server scoped. Its evidence
// identifiers can be opened in the existing evidence endpoint, which repeats
// the same server authorization checks. No raw source path is returned.
func (r *ShadowLedger) ListShadowHistory(ctx context.Context,guildID,serverID int64,before *int64,limit int)([]ShadowHistoryRow,error){
 if r==nil||r.pool==nil{return nil,errors.New("C.A.S.E. shadow database unavailable")}
 if guildID<=0||serverID<=0||limit<1||limit>50{return nil,errors.New("invalid history scope or bound")}
 if before!=nil&&*before<=0{return nil,errors.New("invalid history cursor")}
 rows,err:=r.pool.Query(ctx,`SELECT e.id,e.detector_id,e.detector_version,e.status,e.reason_codes,e.created_at,
  COALESCE(array_agg(link.evidence_id ORDER BY link.evidence_id) FILTER (WHERE link.evidence_id IS NOT NULL),'{}'::bigint[])
 FROM case_shadow_evaluations e
 LEFT JOIN case_shadow_evaluation_evidence link
 ON link.guild_id=e.guild_id AND link.server_id=e.server_id AND link.evaluation_id=e.id
 WHERE e.guild_id=$1 AND e.server_id=$2 AND ($3::bigint IS NULL OR e.id<$3)
 GROUP BY e.id
 ORDER BY e.id DESC LIMIT $4`,guildID,serverID,before,limit)
 if err!=nil{return nil,err}
 defer rows.Close()
 out:=make([]ShadowHistoryRow,0)
 for rows.Next(){
  var item ShadowHistoryRow
  if err=rows.Scan(&item.ID,&item.DetectorID,&item.Version,&item.Status,&item.ReasonCodes,&item.CreatedAt,&item.EvidenceIDs);err!=nil{return nil,err}
  out=append(out,item)
 }
 return out,rows.Err()
}


// GetShadowHistoryByID reads one exact scoped diagnostic even after it falls
// beyond the newest history page. It has no runtime caller or write path.
func (r *ShadowLedger) GetShadowHistoryByID(ctx context.Context,guildID,serverID,evaluationID int64)(ShadowHistoryRow,error){
 if r==nil||r.pool==nil{return ShadowHistoryRow{},errors.New("C.A.S.E. shadow database unavailable")}
 if guildID<=0||serverID<=0||evaluationID<=0{return ShadowHistoryRow{},errors.New("invalid exact history scope")}
 var item ShadowHistoryRow
 err:=r.pool.QueryRow(ctx,`SELECT e.id,e.detector_id,e.detector_version,e.status,e.reason_codes,e.created_at,
  COALESCE(array_agg(link.evidence_id ORDER BY link.evidence_id) FILTER (WHERE link.evidence_id IS NOT NULL),'{}'::bigint[])
 FROM case_shadow_evaluations e
 LEFT JOIN case_shadow_evaluation_evidence link
 ON link.guild_id=e.guild_id AND link.server_id=e.server_id AND link.evaluation_id=e.id
 WHERE e.guild_id=$1 AND e.server_id=$2 AND e.id=$3
 GROUP BY e.id`,guildID,serverID,evaluationID).Scan(
  &item.ID,&item.DetectorID,&item.Version,&item.Status,&item.ReasonCodes,&item.CreatedAt,&item.EvidenceIDs)
 if err!=nil{return ShadowHistoryRow{},err}
 return item,nil
}
