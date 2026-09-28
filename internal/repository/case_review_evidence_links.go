package repository

import (
 "context"
 "errors"

 "github.com/jackc/pgx/v5/pgxpool"
)

// CaseEvidenceLinkReader returns only opaque evidence IDs already linked to an
// admitted case. It never exposes raw ADM, players, coordinates or a source path.
// Individual evidence detail remains under the existing protected evidence API.
type CaseEvidenceLinkReader struct {pool *pgxpool.Pool}
func NewCaseEvidenceLinkReader(pool *pgxpool.Pool)*CaseEvidenceLinkReader{
 return &CaseEvidenceLinkReader{pool:pool}
}

// ListAuthorized performs the actor membership and exact installation/server/
// guild check in the SAME SELECT as the case/evidence join. It cannot create a
// case, change a review, publish an alert or activate a detector.
func (r *CaseEvidenceLinkReader) ListAuthorized(ctx context.Context,
 scope CaseReviewScope,caseID,actorUserID int64,before *int64,limit int)([]int64,error){
 if r==nil||r.pool==nil{return nil,errors.New("C.A.S.E. evidence link reader unavailable")}
 if scope.GuildID<=0||scope.ServerID<=0||scope.InstallationID<=0||
  caseID<=0||actorUserID<=0||limit<1||limit>50{
  return nil,errors.New("invalid case evidence link scope")
 }
 if before!=nil&&*before<=0{return nil,errors.New("invalid evidence cursor")}
 rows,err:=r.pool.Query(ctx,`
 SELECT ce.evidence_id
 FROM case_review_evidence ce
 JOIN case_review_cases c ON c.guild_id=ce.guild_id
  AND c.server_id=ce.server_id AND c.installation_id=ce.installation_id
  AND c.id=ce.case_id
 WHERE c.guild_id=$1 AND c.server_id=$2 AND c.installation_id=$3
  AND c.id=$4
  AND ($5::bigint IS NULL OR ce.evidence_id<$5)
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
 ORDER BY ce.evidence_id DESC LIMIT $7`,
 scope.GuildID,scope.ServerID,scope.InstallationID,caseID,before,actorUserID,limit)
 if err!=nil{return nil,err}
 defer rows.Close()
 out:=make([]int64,0,limit)
 for rows.Next(){var id int64;if err=rows.Scan(&id);err!=nil{return nil,err};out=append(out,id)}
 return out,rows.Err()
}
