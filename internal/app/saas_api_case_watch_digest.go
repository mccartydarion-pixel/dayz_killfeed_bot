package app

import (
 "context"
 "fmt"
 "log/slog"
 "net/http"
 "time"

 "github.com/yourname/dayz-killfeed/internal/casebilling"
 "github.com/yourname/dayz-killfeed/internal/discord"
 "github.com/yourname/dayz-killfeed/internal/routing"
)

const codeCaseStaffRouteRequired = "CASE_STAFF_ROUTE_REQUIRED"

func init(){
 httpStatusForCode[codeCaseStaffRouteRequired]=http.StatusConflict
}

// Explicit Watch feature: queue an aggregate of already-persisted ADM evidence
// for the selected server's configured ADMIN_ALERTS destination. It does not
// start a collector, infer cheating, reveal player identities, or auto-ban.
//
// There are TWO independent paid checks: the HTTP request must pass now and
// the Discord worker checks again just before sending. A queued summary is
// never a reusable entitlement token.
func (a *App) handleAntiCheatWatchDigest(w http.ResponseWriter,r *http.Request){
 ac,ok:=a.requireCasePremium(w,r,casebilling.CapWatch)
 if !ok{return}
 if a.DB==nil || a.DB.Pool==nil || a.AdminAlerts==nil || a.ChannelRoutes==nil {
  writeSaaSError(w,codeCaseAccessUnavailable,"C.A.S.E. staff digest unavailable");return
 }
 if !enforceRateLimit(w,a.saasAdminActionLimiter,rateLimitKey(r)){return}
 serverID:=*ac.scope.ServerID
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
 defer cancel()
 // An omitted route is a configuration requirement, not permission to
 // fallback to a generic or public channel.
 channel,found,routeErr:=a.ChannelRoutes.Resolve(ctx,ac.scope.GuildID,serverID,routing.RouteAdminAlerts)
 if routeErr!=nil{
  slog.Warn("component=case","event","watch_digest_route_failed","server_id",serverID,"err",routeErr.Error())
  writeSaaSError(w,codeCaseAccessUnavailable,"staff channel lookup unavailable");return
 }
 if !found || channel==""{
  writeSaaSError(w,codeCaseStaffRouteRequired,"configure an ADMIN_ALERTS staff channel before sharing a Watch digest");return
 }
 now:=time.Now().UTC()
 from:=now.Add(-24*time.Hour)
 var total,hits,kills int64
 err:=a.DB.Pool.QueryRow(ctx,`
 SELECT COUNT(*),
        COUNT(*) FILTER (WHERE event_type='PLAYER_HIT'),
        COUNT(*) FILTER (WHERE event_type='PLAYER_KILL')
 FROM case_evidence_events
 WHERE guild_id=$1 AND server_id=$2 AND ingested_at >=$3 AND ingested_at <$4
 `,ac.scope.GuildID,serverID,from,now).Scan(&total,&hits,&kills)
 if err!=nil{
  slog.Warn("component=case","event","watch_digest_count_failed","server_id",serverID,"err",err.Error())
  writeSaaSError(w,codeCaseAccessUnavailable,"could not read C.A.S.E. source evidence");return
 }
 // Rate-limit successful on-demand requests, keyed by server rather than
 // user; prevent one staff actor from bypassing another's cooldown.
 key:=fmt.Sprintf("case-watch:%d:%d:%d",ac.scope.OrganizationID,ac.scope.InstallationID,serverID)
 if !enforceRateLimit(w,a.caseWatchDigestLimiter,key){return}
 coverage:="Opt-in collector configured; source lines only"
 if !caseEvidenceEnabledForServer(serverID){
  coverage="Collector disabled or historical; counts do not represent all gameplay"
 }
 alert:=discord.AdminAlert{
  GuildRowID:ac.scope.GuildID,ServerID:serverID,
  Kind:discord.AlertKindCaseWatchDigest,
  Headline:"C.A.S.E. WATCH OBSERVATIONS",
  At:now,
  Fields:[][2]string{
   {"Window","Past 24 hours, by ingestion time"},
   {"Persisted source lines",fmt.Sprintf("%d",total)},
   {"Persisted hits",fmt.Sprintf("%d",hits)},
   {"Persisted kills",fmt.Sprintf("%d",kills)},
   {"Coverage",coverage},
  },
 }
 scope:=discord.CaseWatchScope{
  OrganizationID:ac.scope.OrganizationID,
  InstallationID:ac.scope.InstallationID,
  GameServerID:serverID,
 }
 if !a.AdminAlerts.QueueCaseWatchDigest(alert,scope){
  writeSaaSError(w,codeCaseAccessUnavailable,"C.A.S.E. staff digest queue is full");return
 }
 a.recordAudit(ctx,ac,"CASE_WATCH_DIGEST_QUEUED","","","queued",nil,
  map[string]any{"serverId":serverID,"sourceLines":total})
 writeSaaSJSON(w,http.StatusAccepted,map[string]any{
  "queued":true,"serverId":serverID,
  "mode":"OBSERVATION_ONLY",
  "delivery":"ASYNC_RECHECKED",
  "note":"A queued request does not confirm Discord delivery. Coverage is limited to persisted source lines.",
 })
}
