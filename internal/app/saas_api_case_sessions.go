package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type caseSessionPage struct {
	caseintel.Reconstruction
	ServerID int64 `json:"serverId"`
	NextCursor *string `json:"nextCursor"`
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
	out:=caseSessionPage{
		Reconstruction:caseintel.Reconstruct(playerID,observations,limit,next!=nil||before!=nil),
		ServerID:*ac.scope.ServerID,
	}
	if next!=nil{
		cursor:=strconv.FormatInt(*next,10)
		out.NextCursor=&cursor
	}
	a.recordAudit(ctx,ac,"CASE_SESSIONS_VIEWED","","","success",nil,
		map[string]any{"observations":len(observations),"sources":len(out.Sources),"hasMore":next!=nil})
	writeSaaSJSON(w,http.StatusOK,out)
}
