package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

const codeCaseStaffRouteRequired = "CASE_STAFF_ROUTE_REQUIRED"

func init(){httpStatusForCode[codeCaseStaffRouteRequired]=http.StatusConflict}

// Explicit staff request only. The accepted record is a DURABLE DB enqueue,
// never a claim of Discord delivery. PostgreSQL serializes requests across
// replicas and enforces the per-server rolling-hour duplicate suppression.
func (a *App) handleAntiCheatWatchDigest(w http.ResponseWriter,r *http.Request){
	ac,ok:=a.requireCasePremium(w,r,casebilling.CapWatch)
	if !ok{return}
	if a.DB==nil || a.DB.Pool==nil || a.CaseDigestOutbox==nil ||
		a.SaaSChannelRoutes==nil || a.ClientAdmin==nil {
		writeSaaSError(w,codeCaseAccessUnavailable,"C.A.S.E. digest unavailable");return
	}
	if !enforceRateLimit(w,a.saasAdminActionLimiter,rateLimitKey(r)){return}
	serverID:=*ac.scope.ServerID
	ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
	defer cancel()
	// Do not use the route cache for a privacy-sensitive paid operation.
	channel,found,routeErr:=a.SaaSChannelRoutes.ResolveChannel(ctx,ac.scope.GuildID,serverID,routing.RouteAdminAlerts)
	if routeErr!=nil{
		slog.Warn("component=case","event","watch_digest_route_failed","server_id",serverID,"err",routeErr.Error())
		writeSaaSError(w,codeCaseAccessUnavailable,"staff channel lookup unavailable");return
	}
	if !found || channel=="" {
		writeSaaSError(w,codeCaseStaffRouteRequired,"configure a private ADMIN_ALERTS channel first");return
	}
	if err:=a.caseWatchPrivateDestination(ctx,ac.scope.DiscordGuildID,channel);err!=nil{
		writeSaaSError(w,codeCaseStaffRouteRequired,
			"ADMIN_ALERTS must deny public viewing and allow Champion to send embeds");return
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
		writeSaaSError(w,codeCaseAccessUnavailable,"could not read source evidence");return
	}
	// Local defense in depth; the durable DB admission owns cross-replica cooldown.
	key:=fmt.Sprintf("case-watch:%d:%d:%d",ac.scope.OrganizationID,ac.scope.InstallationID,serverID)
	if !enforceRateLimit(w,a.caseWatchDigestLimiter,key){return}
	id,err:=a.CaseDigestOutbox.Enqueue(ctx,repository.CaseDigestInput{
		OrganizationID:ac.scope.OrganizationID,InstallationID:ac.scope.InstallationID,
		GuildID:ac.scope.GuildID,GameServerID:serverID,RequesterUserID:ac.user.ID,
		WindowStart:from,WindowEnd:now,
		SourceLines:total,HitLines:hits,KillLines:kills,
		CollectorEnabled:caseEvidenceEnabledForServer(serverID),
	})
	if errors.Is(err,repository.ErrCaseDigestCooldown){
		writeSaaSJSON(w,http.StatusTooManyRequests,apiErrorEnvelope{Error:apiError{
			Code:"CASE_DIGEST_COOLDOWN",Message:"a Watch digest was already requested for this server within the last hour",
		}});return
	}
	if errors.Is(err,repository.ErrCaseDigestScope){
		writeSaaSError(w,codeCasePremiumRequired,"selected server binding has changed");return
	}
	if err!=nil{
		slog.Warn("component=case","event","watch_digest_enqueue_failed","server_id",serverID,"err",err.Error())
		writeSaaSError(w,codeCaseAccessUnavailable,"could not persist staff digest");return
	}
	a.recordAudit(ctx,ac,"CASE_WATCH_DIGEST_QUEUED","","","queued",nil,
		map[string]any{"serverId":serverID,"digestId":id,"sourceLines":total})
	writeSaaSJSON(w,http.StatusAccepted,map[string]any{
		"queued":true,"deliveryId":id,"status":"READY","serverId":serverID,
		"mode":"OBSERVATION_ONLY","delivery":"DURABLE_PENDING",
		"note":"The request is persisted, not delivered. Payment, server binding, route and privacy will be rechecked before sending.",
	})
}

// A scoped receipt lets staff distinguish accepted, delivered, blocked and
// ambiguous Discord outcomes. A row is not visible after switching servers.
func (a *App) handleAntiCheatWatchDigestReceipt(w http.ResponseWriter,r *http.Request){
	ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
	if !ok{return}
	if ac.scope.ServerID==nil || a.CaseDigestOutbox==nil {
		writeSaaSError(w,codeCaseAccessUnavailable,"digest receipt unavailable");return
	}
	id,good:=pathInt64(w,r,"deliveryID")
	if !good{return}
	if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
	ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
	defer cancel()
	row,err:=a.CaseDigestOutbox.GetScoped(ctx,ac.scope.OrganizationID,ac.scope.InstallationID,id)
	if err!=nil{
		writeSaaSError(w,codeCaseAccessUnavailable,"could not load delivery receipt");return
	}
	if row==nil || row.GameServerID!=*ac.scope.ServerID {
		writeSaaSError(w,codeNotFound,"digest receipt not found for selected server");return
	}
	writeSaaSJSON(w,http.StatusOK,map[string]any{
		"deliveryId":row.ID,"serverId":row.GameServerID,
		"status":row.Status,"requestedAt":row.RequestedAt,
		"sentAt":row.SentAt,"channelId":row.DiscordChannelID,
		"messageId":row.DiscordMessageID,"reasonCode":row.ReasonCode,
		"attempts":row.Attempts,
	})
}
