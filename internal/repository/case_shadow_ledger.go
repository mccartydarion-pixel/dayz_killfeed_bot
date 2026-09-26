package repository

import (
 "context"
 "errors"
 "fmt"
 "sort"

 "github.com/jackc/pgx/v5"
 "github.com/jackc/pgx/v5/pgxpool"
 "github.com/yourname/dayz-killfeed/internal/caseintel"
)

// ShadowLedger is an inert, explicit-call persistence primitive. No caller is
// wired to production polling, web requests or the killfeed. It can store only
// BLOCKED prerequisite diagnostics, never flags, scores or sanctions.
type ShadowLedger struct {pool *pgxpool.Pool}
func NewShadowLedger(pool *pgxpool.Pool)*ShadowLedger{return &ShadowLedger{pool:pool}}
var ErrShadowDisabled=errors.New("C.A.S.E. shadow ledger disabled")

type BlockedShadowInput struct {
 Enabled bool // explicit, separately controlled test/staging gate; defaults false
 GuildID,ServerID int64
 DetectorID,Version string
 EvidenceIDs []int64
}
func (r *ShadowLedger) RecordBlocked(ctx context.Context,in BlockedShadowInput)(int64,error){
 if !in.Enabled{return 0,ErrShadowDisabled}
 if r==nil||r.pool==nil{return 0,errors.New("C.A.S.E. shadow database unavailable")}
 if in.GuildID<=0||in.ServerID<=0||len(in.EvidenceIDs)==0||
  len(in.EvidenceIDs)>200{
  return 0,errors.New("invalid bounded shadow input")
 }
 defs:=caseintel.Registry()
 var def *caseintel.DetectorDefinition
 for i:=range defs{if defs[i].ID==in.DetectorID&&defs[i].Version==in.Version&&defs[i].Mode=="BLOCKED"{def=&defs[i];break}}
 if def==nil{return 0,errors.New("detector not registered as blocked")}
 evaluation:=caseintel.EvaluatePrerequisites(*def,caseintel.QualityReport{})
 if evaluation.Status!="BLOCKED"||len(evaluation.Blockers)==0||len(evaluation.Findings)!=0||evaluation.RiskScore!=nil{
  return 0,errors.New("detector is not safe to persist as blocked")
 }
 reasons:=make([]string,0,len(evaluation.Blockers))
 for _,b:=range evaluation.Blockers{if b.Status!="UNSATISFIED"||b.Name==""{return 0,errors.New("invalid prerequisite state")};reasons=append(reasons,b.Name)}
 sort.Strings(reasons)
 fp,err:=caseintel.EvidenceFingerprint(in.GuildID,in.ServerID,in.DetectorID,in.Version,in.EvidenceIDs)
 if err!=nil{return 0,err}
 // Every evidence ID is verified under the exact tenant and game-server scope
 // in the SAME transaction as the ledger/link writes. The composite foreign
 // keys also prevent accidental cross-tenant linking.
 tx,err:=r.pool.Begin(ctx);if err!=nil{return 0,err};defer tx.Rollback(ctx)
 var n int
 err=tx.QueryRow(ctx,`SELECT COUNT(*) FROM case_evidence_events
   WHERE guild_id=$1 AND server_id=$2 AND id=ANY($3::bigint[])`,
   in.GuildID,in.ServerID,in.EvidenceIDs).Scan(&n)
 if err!=nil{return 0,err}
 if n!=len(in.EvidenceIDs){return 0,errors.New("evidence missing or outside selected server")}
 // The fingerprint binds exact evidence; a repeated call is idempotent.
 // Blocker content is immutable after initial insertion.
 var id int64
 err=tx.QueryRow(ctx,`INSERT INTO case_shadow_evaluations
   (guild_id,server_id,detector_id,detector_version,fingerprint,status,reason_codes)
   VALUES($1,$2,$3,$4,$5,'BLOCKED',$6)
   ON CONFLICT(guild_id,server_id,detector_id,detector_version,fingerprint)
   DO NOTHING RETURNING id`,in.GuildID,in.ServerID,in.DetectorID,in.Version,fp,reasons).Scan(&id)
 if errors.Is(err,pgx.ErrNoRows){
  err=tx.QueryRow(ctx,`SELECT id FROM case_shadow_evaluations
   WHERE guild_id=$1 AND server_id=$2 AND detector_id=$3
    AND detector_version=$4 AND fingerprint=$5`,
    in.GuildID,in.ServerID,in.DetectorID,in.Version,fp).Scan(&id)
 }
 if err!=nil{return 0,fmt.Errorf("persist blocked shadow evaluation: %w",err)}
 for _,evidenceID:=range in.EvidenceIDs{
  _,err=tx.Exec(ctx,`INSERT INTO case_shadow_evaluation_evidence
   (guild_id,server_id,evaluation_id,evidence_id)
   VALUES($1,$2,$3,$4) ON CONFLICT(evaluation_id,evidence_id) DO NOTHING`,
   in.GuildID,in.ServerID,id,evidenceID)
  if err!=nil{return 0,err}
 }
 if err=tx.Commit(ctx);err!=nil{return 0,err}
 return id,nil
}
