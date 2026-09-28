package repository

import (
 "context"
 "errors"
 "time"
)

// SyntheticAuthorizedAuditRead is fixture-only. A future route must authenticate
// the actor and selected installation before any repository call.
type SyntheticAuthorizedAuditRead struct {
 FixtureOnly bool
 Scope CaseReviewScope
 CaseID int64
 ActorUserID int64
 Before *int64
 Limit int
}

// SyntheticAuditSummary excludes notes, player data and raw evidence. An
// authorized operator can review the immutable state transition chronology.
type SyntheticAuditSummary struct {
 ID int64
 FromStatus string
 ToStatus string
 ReasonCode string
 CreatedAt time.Time
}

// ListAuthorizedAuditSynthetic checks membership and exact installation scope
// in the same bounded statement as the audit rows. It has no runtime caller.
func (r *CaseReviewReader) ListAuthorizedAuditSynthetic(ctx context.Context,in SyntheticAuthorizedAuditRead)([]SyntheticAuditSummary,error){
 if !in.FixtureOnly{return nil,ErrCASEReviewFixtureDisabled}
 if r==nil||r.pool==nil{return nil,errors.New("C.A.S.E. review database unavailable")}
 if in.Scope.GuildID<=0||in.Scope.ServerID<=0||in.Scope.InstallationID<=0||
  in.CaseID<=0||in.ActorUserID<=0||in.Limit<1||in.Limit>50{
  return nil,errors.New("invalid authorized audit history request")
 }
 if in.Before!=nil&&*in.Before<=0{return nil,errors.New("invalid audit history cursor")}
 rows,err:=r.pool.Query(ctx,`
 SELECT a.id,a.from_status,a.to_status,a.reason_code,a.created_at
 FROM case_review_audit a
 JOIN case_review_cases c ON c.id=a.case_id AND c.guild_id=a.guild_id
  AND c.server_id=a.server_id AND c.installation_id=a.installation_id
 WHERE c.guild_id=$1 AND c.server_id=$2 AND c.installation_id=$3 AND c.id=$4
  AND ($5::bigint IS NULL OR a.id<$5)
  AND EXISTS (
   SELECT 1 FROM installations i
   JOIN discord_guild_connections dc ON dc.id=i.discord_guild_connection_id
   JOIN game_servers gs ON gs.id=i.game_server_id
   JOIN organization_members m ON m.organization_id=i.organization_id
   WHERE i.id=c.installation_id AND i.game_server_id=c.server_id
    AND dc.guild_id=c.guild_id AND gs.guild_id=c.guild_id
    AND gs.organization_id=i.organization_id
    AND dc.organization_id=i.organization_id
    AND m.user_id=$6 AND m.role IN ('OWNER','ADMIN')
  )
 ORDER BY a.id DESC LIMIT $7`,
 in.Scope.GuildID,in.Scope.ServerID,in.Scope.InstallationID,in.CaseID,
 in.Before,in.ActorUserID,in.Limit)
 if err!=nil{return nil,err}
 defer rows.Close()
 out:=make([]SyntheticAuditSummary,0,in.Limit)
 for rows.Next(){
  var v SyntheticAuditSummary
  if err=rows.Scan(&v.ID,&v.FromStatus,&v.ToStatus,&v.ReasonCode,&v.CreatedAt);err!=nil{return nil,err}
  out=append(out,v)
 }
 return out,rows.Err()
}
