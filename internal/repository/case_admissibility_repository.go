package repository

import (
 "context"
 "errors"
 "fmt"

 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/jackc/pgx/v5"
)

// AuditCaseEvidenceAdmissibility reads a bounded tenant/server-specific slice
// of persisted, filtered ADM evidence. No new collector, write, player profile,
// detector activation or route is created. Coordinate completeness is counted
// per role; never coalesce X from one role and Z from another.
func (r *CaseEvidenceRepository) AuditCaseEvidenceAdmissibility(ctx context.Context,
 guildID,serverID int64,limit int)(caseintel.AdmissibilityReport,error){
 if r==nil||r.pool==nil{return caseintel.AdmissibilityReport{},errors.New("C.A.S.E. evidence repository unavailable")}
 if guildID<=0||serverID<=0||limit<1||limit>500{
  return caseintel.AdmissibilityReport{},errors.New("invalid evidence scope or bound")
 }
 // A single read-only snapshot prevents partial reads across collector writes
 // and prevents this audit from writing even if extended in the future.
 tx,err:=r.pool.BeginTx(ctx,pgx.TxOptions{IsoLevel:pgx.RepeatableRead,AccessMode:pgx.ReadOnly})
 if err!=nil{return caseintel.AdmissibilityReport{},errors.New("read-only evidence snapshot unavailable")}
 defer tx.Rollback(ctx)
 rows,err:=tx.Query(ctx,`SELECT id,source_id,source_end_offset,line_sha256,event_type,adm_clock,
 subject_x,subject_z,actor_x,actor_z,target_x,target_z FROM case_evidence_events
 WHERE guild_id=$1 AND server_id=$2 ORDER BY id DESC LIMIT $3`,guildID,serverID,limit+1)
 if err!=nil{return caseintel.AdmissibilityReport{},fmt.Errorf("read bounded source evidence: %w",err)}
 samples:=make([]caseintel.AdmissibilitySample,0,limit+1)
 for rows.Next(){
  var sample caseintel.AdmissibilitySample
  if err=rows.Scan(&sample.EvidenceID,&sample.SourceID,&sample.SourceEndOffset,
   &sample.LineSHA256,&sample.EventType,&sample.ADMClock,&sample.X,&sample.Z,
   &sample.ActorX,&sample.ActorZ,&sample.TargetX,&sample.TargetZ);err!=nil{
   rows.Close();return caseintel.AdmissibilityReport{},errors.New("invalid persisted evidence row")
  }
  samples=append(samples,sample)
 }
 err=rows.Err()
 rows.Close()
 if err!=nil{return caseintel.AdmissibilityReport{},errors.New("persisted evidence read failed")}
 truncated:=len(samples)>limit
 if truncated {samples=samples[:limit]}
 if err=tx.Commit(ctx);err!=nil{
  return caseintel.AdmissibilityReport{},errors.New("read-only evidence snapshot failed")
 }
 return caseintel.AuditAdmissibility(samples,limit,truncated)
}
