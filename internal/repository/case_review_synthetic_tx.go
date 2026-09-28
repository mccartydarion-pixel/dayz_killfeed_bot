package repository

import (
 "context"
 "errors"
 "fmt"
 "strings"
 "time"

 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/jackc/pgx/v5"
 "github.com/jackc/pgx/v5/pgxpool"
)

// ErrCASEReviewFixtureDisabled identifies an intentionally inaccessible,
// synthetic-only transaction. There is no runtime caller, route or worker.
var ErrCASEReviewFixtureDisabled = errors.New("C.A.S.E. synthetic review transaction disabled")

// SyntheticReviewInput is NOT a live staff authorization contract. FixtureOnly
// and CallerCapabilityVerified are caller assertions, not trusted credentials.
// Production must authenticate the actor and selected installation in the HTTP
// layer and independently recheck the exact permission in the transaction.
type SyntheticReviewInput struct {
 FixtureOnly bool
 CallerCapabilityVerified bool
 Scope CaseReviewScope
 CaseID int64
 ActorUserID int64
 ActionKey string // exactly 64 lowercase hex characters, opaque fixture identity
 ExpectedStatus string
 ToStatus string
 ReasonCode string
 Note string
 At time.Time
}

// CaseReviewMutation is deliberately not exposed through any HTTP endpoint.
// Its SQL still verifies the actor's same-organization OWNER/ADMIN membership,
// the installation, and a row-locked scoped case before mutating a fixture.
type CaseReviewMutation struct { pool *pgxpool.Pool }

func NewCaseReviewMutation(pool *pgxpool.Pool) *CaseReviewMutation {
 return &CaseReviewMutation{pool:pool}
}

func reviewValidKey(s string) bool {
 if len(s)!=64 {return false}
 for _,r:=range s {
  if (r<'0'||r>'9')&&(r<'a'||r>'f'){return false}
 }
 return true
}

func reviewTransition(from,to string) bool {
 switch from {
 case "PENDING_REVIEW":return to=="DISMISSED"||to=="REVIEWED"
 case "REVIEWED":return to=="RESOLVED"
 default:return false
 }
}

// ApplySynthetic atomically updates a neutral fixture review state and appends
// exactly one audit entry. An action key replays only when every review field
// matches; collisions fail closed. This is not live finding admission.
func (r *CaseReviewMutation) ApplySynthetic(ctx context.Context,in SyntheticReviewInput)(bool,error){
 if !in.FixtureOnly||!in.CallerCapabilityVerified{return false,ErrCASEReviewFixtureDisabled}
 return r.applyReviewTransaction(ctx,CaseReviewAction{
  Scope:in.Scope,CaseID:in.CaseID,ActorUserID:in.ActorUserID,
  ActionKey:in.ActionKey,ExpectedStatus:in.ExpectedStatus,
  ToStatus:in.ToStatus,ReasonCode:in.ReasonCode,Note:in.Note,At:in.At,
 },false)
}

// CaseReviewAction is the trusted server-side transaction input, not a request
// DTO. The real handler must resolve the actor and selected installation via
// requireCapability and never accept caller-provided actor/scope IDs.
type CaseReviewAction struct {
 Scope CaseReviewScope
 CaseID,ActorUserID int64
 ActionKey,ExpectedStatus,ToStatus,ReasonCode,Note string
 At time.Time
}

// ApplyReviewed performs the same DB-enforced exact-installation membership,
// immutable replay and atomic audit/status transition as the fixture method.
// It has no HTTP/runtime caller yet and does NOT create cases or outbox rows.
// A separate reviewed, default-off route must independently authenticate the
// actor, establish case/evidence admission, and require explicit launch approval.
func (r *CaseReviewMutation) ApplyReviewed(ctx context.Context,in CaseReviewAction)(bool,error){
 return r.applyReviewTransaction(ctx,in,true)
}

func (r *CaseReviewMutation) applyReviewTransaction(ctx context.Context,in CaseReviewAction,requireValidated bool)(bool,error){
 if r==nil||r.pool==nil{return false,errors.New("case review database unavailable")}
 if in.Scope.GuildID<=0||in.Scope.ServerID<=0||in.Scope.InstallationID<=0||
  in.CaseID<=0||in.ActorUserID<=0||!reviewValidKey(in.ActionKey)||
  !reviewTransition(in.ExpectedStatus,in.ToStatus)||
  in.At.IsZero()||len(strings.TrimSpace(in.Note))==0||
  len([]rune(in.Note))>500||strings.ContainsAny(in.Note,"@\r\n<>")||
  !reviewReasonValid(in.ReasonCode,in.ToStatus) {
  return false,errors.New("invalid case review request")
 }
 tx,err:=r.pool.BeginTx(ctx,pgx.TxOptions{})
 if err!=nil{return false,err}
 defer tx.Rollback(ctx)

 // Both staff membership and installation ownership are checked from the
 // database, not inferred from an opaque actor ID or the client's scope.
 // Hold a membership row SHARE lock through audit and status commit. A role
 // UPDATE or membership DELETE must serialize against this review; otherwise
 // revocation could commit while review waits for its case-row lock.
 // Lock order: membership first, then case. Replays also take the lock.
 var authorized bool
 err=tx.QueryRow(ctx,`
 SELECT EXISTS (
  SELECT 1 FROM installations i
  JOIN discord_guild_connections dc ON dc.id=i.discord_guild_connection_id
  JOIN game_servers gs ON gs.id=i.game_server_id
  JOIN organization_members m ON m.organization_id=i.organization_id
  WHERE i.id=$1 AND i.game_server_id=$2 AND dc.guild_id=$3
   AND gs.guild_id=$3 AND gs.organization_id=i.organization_id
   AND dc.organization_id=i.organization_id
   AND m.user_id=$4 AND m.role IN ('OWNER','ADMIN')
  FOR SHARE OF m
 )`,in.Scope.InstallationID,in.Scope.ServerID,in.Scope.GuildID,in.ActorUserID).Scan(&authorized)
 if err!=nil{return false,fmt.Errorf("verify case review membership: %w",err)}
 if !authorized{return false,errors.New("actor not authorized for selected installation")}

 var state,detectorID,detectorVersion string
 var openedAt,updatedAt time.Time
 err=tx.QueryRow(ctx,`
  SELECT status,detector_id,detector_version,created_at,updated_at
  FROM case_review_cases
  WHERE id=$1 AND guild_id=$2 AND server_id=$3 AND installation_id=$4
  FOR UPDATE`,in.CaseID,in.Scope.GuildID,in.Scope.ServerID,in.Scope.InstallationID).
  Scan(&state,&detectorID,&detectorVersion,&openedAt,&updatedAt)
 if errors.Is(err,pgx.ErrNoRows){return false,errors.New("review case not found in exact scope")}
 if err!=nil{return false,err}
 if requireValidated {
  // An opt-in review handler cannot turn a legacy fixture, unknown detector
  // or unlinked source into an allegation merely by changing review state.
  validated:=false
  for _,d:=range caseintel.Registry(){
   if d.ID==detectorID && d.Version==detectorVersion && d.Mode=="VALIDATED_SHADOW"{
    validated=true;break
   }
  }
  if !validated{return false,errors.New("case detector not independently validated")}
  var linked bool
  err=tx.QueryRow(ctx,`SELECT EXISTS (
   SELECT 1 FROM case_review_evidence ev
   JOIN case_evidence_events e ON e.id=ev.evidence_id
    AND e.guild_id=ev.guild_id AND e.server_id=ev.server_id
   WHERE ev.guild_id=$1 AND ev.server_id=$2 AND ev.installation_id=$3
    AND ev.case_id=$4
  )`,in.Scope.GuildID,in.Scope.ServerID,in.Scope.InstallationID,in.CaseID).Scan(&linked)
  if err!=nil||!linked{return false,errors.New("case has no verified linked evidence")}
 }

 // Check the replay after acquiring the case lock and verifying live membership.
 // Idempotence cannot be used as an authorization bypass after role revocation.
 var previousFrom,previousTo,previousReason,previousNote string
 var previousActor int64
 var previousTime time.Time
 err=tx.QueryRow(ctx,`
 SELECT from_status,to_status,reason_code,note,actor_user_id,created_at
 FROM case_review_audit
 WHERE guild_id=$1 AND server_id=$2 AND installation_id=$3
  AND case_id=$4 AND action_key=$5
 `,in.Scope.GuildID,in.Scope.ServerID,in.Scope.InstallationID,in.CaseID,in.ActionKey).
 Scan(&previousFrom,&previousTo,&previousReason,&previousNote,&previousActor,&previousTime)
 if err==nil {
  if previousFrom==in.ExpectedStatus&&previousTo==in.ToStatus&&
   previousReason==in.ReasonCode&&previousNote==in.Note&&
   previousActor==in.ActorUserID&&previousTime.Equal(in.At.UTC()){
   return false,tx.Commit(ctx)
  }
  return false,errors.New("review action key collision")
 }
 if !errors.Is(err,pgx.ErrNoRows){return false,err}
 if in.At.UTC().Before(openedAt)||in.At.UTC().Before(updatedAt){
  return false,errors.New("review time precedes case history")
 }
 if state!=in.ExpectedStatus{return false,errors.New("review status changed")}
 if !reviewTransition(state,in.ToStatus){return false,errors.New("review transition refused")}

 // Audit INSERT precedes status UPDATE in the same transaction. If either
 // fails the other rolls back. The SQL schema rejects audit rewrites/deletes.
 _,err=tx.Exec(ctx,`
 INSERT INTO case_review_audit
 (guild_id,server_id,installation_id,case_id,action_key,actor_user_id,
  from_status,to_status,reason_code,note,created_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
 `,in.Scope.GuildID,in.Scope.ServerID,in.Scope.InstallationID,in.CaseID,
 in.ActionKey,in.ActorUserID,state,in.ToStatus,in.ReasonCode,in.Note,in.At.UTC())
 if err!=nil{return false,err}
 tag,err:=tx.Exec(ctx,`
 UPDATE case_review_cases SET status=$1,updated_at=$2
 WHERE id=$3 AND guild_id=$4 AND server_id=$5 AND installation_id=$6
 AND status=$7
 `,in.ToStatus,in.At.UTC(),in.CaseID,in.Scope.GuildID,in.Scope.ServerID,
 in.Scope.InstallationID,state)
 if err!=nil{return false,err}
 if tag.RowsAffected()!=1{return false,errors.New("case review compare-and-swap failed")}
 if err=tx.Commit(ctx);err!=nil{return false,err}
 return true,nil
}

func reviewReasonValid(reason,to string)bool{
 switch to{
 case "REVIEWED":return reason=="EVIDENCE_REVIEWED"
 case "DISMISSED":return reason=="INSUFFICIENT_EVIDENCE"||reason=="FALSE_POSITIVE"
 case "RESOLVED":return reason=="STAFF_CLOSED"
 }
 return false
}
