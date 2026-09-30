package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type caseSessionPage struct {
	caseintel.Reconstruction
	ServerID int64 `json:"serverId"`
	NextCursor *string `json:"nextCursor"`
	// LoginShadow is a read-only shadow run of the Suspicious Logins detector
	// over this page. It has no thresholds and cannot notify or enforce.
	LoginShadow caseintel.ShadowLoginReport `json:"loginShadow"`
}

// caseADMTelemetry describes the live ADM feed for one server from the
// running worker's snapshot, with the same freshness rule as the health read.
func (a *App) caseADMTelemetry(serverID int64, now time.Time) caseintel.TelemetrySnapshot {
	a.presenceMu.Lock()
	engine:=a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	feed:=caseintel.TelemetryFeed{MaxAge:2*time.Minute}
	if engine!=nil {
		source:=engine.SourceHealth()
		state,_:=killfeed.ClassifyADMSourceHealth(source,now)
		feed.Available=true
		feed.Verified=state==killfeed.ADMHealthy||state==killfeed.ADMQuiet
		feed.LatestAt=source.LastCycleAt.UTC()
	}
	return caseintel.TelemetrySnapshot{Now:now,PollingDelayLimit:30*time.Second,
		Feeds:map[caseintel.TelemetryKind]caseintel.TelemetryFeed{caseintel.TelemetryADMEvents:feed}}
}

// A reconstruction is a source-ordered view of real ADM evidence, never a
// player verdict. Location-view permission is required because observations
// include scoped positions and player identities.
func (a *App) handleAntiCheatSessions(w http.ResponseWriter, r *http.Request) {
	ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
	if !ok{return}
	if ac.scope.ServerID==nil{
		writeSaaSError(w,codeInvalidRequest,"no DayZ server selected")
		return
	}
	if a.DB==nil||a.DB.Pool==nil{
		writeSaaSError(w,codeInternalError,"C.A.S.E. session data unavailable")
		return
	}
	if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
	query:=r.URL.Query()
	playerID,err:=strconv.ParseInt(query.Get("playerId"),10,64)
	if err!=nil||playerID<=0{
		writeSaaSError(w,codeInvalidRequest,"playerId is required and must be positive")
		return
	}
	var before *int64
	if raw:=query.Get("before");raw!=""{
		value,parseErr:=strconv.ParseInt(raw,10,64)
		if parseErr!=nil||value<=0{
			writeSaaSError(w,codeInvalidRequest,"invalid evidence cursor")
			return
		}
		before=&value
	}
	limit:=200
	if raw:=query.Get("limit");raw!=""{
		value,parseErr:=strconv.Atoi(raw)
		if parseErr!=nil||value<1||value>500{
			writeSaaSError(w,codeInvalidRequest,"limit must be between 1 and 500")
			return
		}
		limit=value
	}
	ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
	defer cancel()
	observations,next,err:=repository.NewCaseEvidenceRepository(a.DB.Pool).
		ListCaseSessionEvidence(ctx,ac.scope.GuildID,*ac.scope.ServerID,playerID,before,limit)
	if err!=nil{
		slog.Warn("component=case","event","session_reconstruction_failed","err",err.Error())
		writeSaaSError(w,codeInternalError,"could not reconstruct C.A.S.E. sessions")
		return
	}
	repo:=repository.NewCaseEvidenceRepository(a.DB.Pool)
	offset,err:=repo.CaseServerUTCOffset(ctx,ac.scope.GuildID,*ac.scope.ServerID)
	if err!=nil{
		slog.Warn("component=case","event","session_clock_read_failed","err",err.Error())
		offset=nil // fail closed: no trusted time, no shadow conclusions
	}
	truncated:=next!=nil||before!=nil
	out:=caseSessionPage{
		Reconstruction:caseintel.Reconstruct(playerID,observations,limit,truncated),
		ServerID:*ac.scope.ServerID,
		LoginShadow:caseintel.ShadowSuspiciousLogins(caseintel.ShadowLoginInput{
			Scope:caseintel.Core8Scope{GuildID:ac.scope.GuildID,InstallationID:ac.scope.InstallationID,ServerID:*ac.scope.ServerID},
			PlayerID:playerID,Events:observations,UTCOffsetMinutes:offset,
			Telemetry:a.caseADMTelemetry(*ac.scope.ServerID,time.Now().UTC()),
			SessionsRetained:caseEvidenceEnabledForServer(*ac.scope.ServerID),WindowTruncated:truncated}),
	}
	if next!=nil{
		cursor:=strconv.FormatInt(*next,10)
		out.NextCursor=&cursor
	}
	a.recordAudit(ctx,ac,"CASE_SESSIONS_VIEWED","","","success",nil,
		map[string]any{"observations":len(observations),"sources":len(out.Sources),"hasMore":next!=nil})
	writeSaaSJSON(w,http.StatusOK,out)
}
