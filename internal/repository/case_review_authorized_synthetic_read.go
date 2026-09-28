package repository

import (
 "context"
 "errors"

)

// SyntheticAuthorizedCaseRead preserves the fixture-only caller contract.
// FixtureOnly is not an HTTP credential; the live read route independently
// authenticates the actor and capability before calling ListAuthorized.
type SyntheticAuthorizedCaseRead struct {
 FixtureOnly bool
 Scope CaseReviewScope
 ActorUserID int64
 Before *int64
 Limit int
}

// ListAuthorizedSynthetic takes the installation, its guild/server and the
// qualifying OWNER/ADMIN member in ONE statement. A separate preflight
// membership read followed by List would permit a role-revocation race.
// The result intentionally contains neutral metadata only, never notes,
// identities, coordinates, raw ADM paths or unreviewed detector details.
func (r *CaseReviewReader) ListAuthorizedSynthetic(ctx context.Context,in SyntheticAuthorizedCaseRead)([]CaseReviewSummary,error){
 if !in.FixtureOnly{return nil,ErrCASEReviewFixtureDisabled}
 return r.ListAuthorized(ctx,in.Scope,in.ActorUserID,in.Before,in.Limit)
}

// ListAuthorized is a read-only queue for an already authenticated actor.
// Membership, installation, guild and server scope are rechecked in the same
// SQL statement as the rows, so a revoked role cannot race a separate preflight.
// The HTTP caller must also enforce its installation capability and rate limit.
func (r *CaseReviewReader) ListAuthorized(ctx context.Context,scope CaseReviewScope,actorUserID int64,before *int64,limit int)([]CaseReviewSummary,error){
 if r==nil||r.pool==nil{return nil,errors.New("C.A.S.E. review database unavailable")}
 if scope.GuildID<=0||scope.ServerID<=0||scope.InstallationID<=0||
  actorUserID<=0||limit<1||limit>50{
  return nil,errors.New("invalid authorized case review request")
 }
 if before!=nil&&*before<=0{return nil,errors.New("invalid case review cursor")}
 rows,err:=r.pool.Query(ctx,`
 SELECT c.id,c.detector_id,c.detector_version,c.status,
  (SELECT COUNT(*) FROM case_review_evidence ev
   WHERE ev.guild_id=c.guild_id AND ev.server_id=c.server_id
    AND ev.installation_id=c.installation_id AND ev.case_id=c.id),
  (SELECT COUNT(*) FROM case_review_audit a
   WHERE a.guild_id=c.guild_id AND a.server_id=c.server_id
    AND a.installation_id=c.installation_id AND a.case_id=c.id),
  c.created_at,c.updated_at
 FROM case_review_cases c
 WHERE c.guild_id=$1 AND c.server_id=$2 AND c.installation_id=$3
 AND ($4::bigint IS NULL OR c.id<$4)
 AND EXISTS (
  SELECT 1 FROM installations i
  JOIN discord_guild_connections dc ON dc.id=i.discord_guild_connection_id
  JOIN game_servers gs ON gs.id=i.game_server_id
  JOIN organization_members m ON m.organization_id=i.organization_id
  WHERE i.id=c.installation_id AND i.game_server_id=c.server_id
   AND dc.guild_id=c.guild_id AND gs.guild_id=c.guild_id
   AND gs.organization_id=i.organization_id
   AND dc.organization_id=i.organization_id
   AND m.user_id=$5 AND m.role IN ('OWNER','ADMIN')
 )
 ORDER BY c.id DESC LIMIT $6`,
 scope.GuildID,scope.ServerID,scope.InstallationID,before,actorUserID,limit)
 if err!=nil{return nil,err}
 defer rows.Close()
 out:=make([]CaseReviewSummary,0,limit)
 for rows.Next(){
  var v CaseReviewSummary
  if err=rows.Scan(&v.ID,&v.DetectorID,&v.DetectorVersion,&v.Status,
   &v.EvidenceCount,&v.AuditCount,&v.CreatedAt,&v.UpdatedAt);err!=nil{return nil,err}
  out=append(out,v)
 }
 return out,rows.Err()
}
