//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func caseSessionFixture(guildID,serverID,offset int64,source,kind string)repository.CaseEvidenceInput{
	return repository.CaseEvidenceInput{
		GuildID:guildID,ServerID:serverID,SourceID:source,SourceEndOffset:offset,
		LineSHA256:fmt.Sprintf("%064x",offset+int64(len(kind))),EventType:kind,ADMClock:"09:43:19",
		Subject:repository.CaseEvidencePerson{DayZID:"case-session-victim",Name:"Victim"},
	}
}
func TestCASESessionAPIReconstructsFromSourceOffsetsAndScopesTenant(t *testing.T){
	w:=newClientAdminWorld(t)
	ctx:=context.Background()
	repo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	source:="dayzps/config/phase2c-test.ADM"
	first:=caseSessionFixture(w.guildID,w.serverID,100,source,"PLAYER_CONNECT")
	if err:=repo.RecordCaseEvidence(ctx,first);err!=nil{t.Fatal(err)}
	hit:=caseSessionFixture(w.guildID,w.serverID,300,source,"PLAYER_HIT")
	hit.Subject=repository.CaseEvidencePerson{}
	hit.Actor=repository.CaseEvidencePerson{DayZID:"case-session-attacker",Name:"Attacker"}
	hit.Target=repository.CaseEvidencePerson{DayZID:"case-session-victim",Name:"Victim"}
	if err:=repo.RecordCaseEvidence(ctx,hit);err!=nil{t.Fatal(err)}
	kill:=caseSessionFixture(w.guildID,w.serverID,200,source,"PLAYER_KILL")
	kill.Subject=repository.CaseEvidencePerson{}
	kill.Actor=hit.Actor;kill.Target=hit.Target;kill.BoundaryKind="DEATH"
	if err:=repo.RecordCaseEvidence(ctx,kill);err!=nil{t.Fatal(err)}
	last:=caseSessionFixture(w.guildID,w.serverID,400,source,"PLAYER_DISCONNECT")
	if err:=repo.RecordCaseEvidence(ctx,last);err!=nil{t.Fatal(err)}
	var playerID int64
	if err:=w.a.DB.Pool.QueryRow(ctx,
		"SELECT id FROM players WHERE guild_id=$1 AND dayz_player_id='case-session-victim'",
		w.guildID).Scan(&playerID);err!=nil{t.Fatal(err)}
	path:=fmt.Sprintf("%s?playerId=%d",w.path("/anti-cheat/sessions"),playerID)
	rr:=w.call(w.a.handleAntiCheatSessions,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusOK{t.Fatalf("owner cannot read sessions: %d %s",rr.Code,rr.Body.String())}
	out:=decodeBody[caseSessionPage](t,rr)
	if out.ServerID!=w.serverID||out.Mode!="RECONSTRUCTION_ONLY"||out.DetectorsEnabled||
		out.Enforcement!="DISABLED"||len(out.Sources)!=1||out.LoginObservations.DetectorsEnabled||
		out.LoginObservations.RepeatedConnectWindows!=0||out.LoginObservations.RestartContext!="UNVERIFIED"{
		t.Fatalf("unsafe session response: %+v",out)
	}
	if sh:=out.LoginShadow;sh.Mode!="SHADOW_OBSERVATION_ONLY"||sh.Result.CanNotify||sh.Result.Enforcement!="DISABLED"||
		len(sh.Result.Findings)!=0||sh.TrustedEvents!=0||sh.UntrustedEvents["BOOT_STAMP_UNAVAILABLE"]!=4{
		t.Fatalf("unsafe login shadow: %+v",sh)
	}
	s:=out.Sources[0]
	if s.ObservationCount!=4||len(s.ConnectionWindows)!=1{t.Fatalf("unexpected source grouping: %+v",s)}
	episode:=s.ConnectionWindows[0]
	if episode.StartReason!="CONNECT"||episode.EndReason!="DISCONNECT"||
		len(episode.Lives)!=1||episode.Lives[0].EndReason!="RECORDED_DEATH"{
		t.Fatalf("wrong explicit session/life boundaries: %+v",episode)
	}
	if episode.Observations[1].SourceEndOffset!=200||episode.Observations[2].SourceEndOffset!=300{
		t.Fatalf("did not use source offsets: %+v",episode.Observations)
	}
	if len(episode.Observations[2].Notes)==0{
		t.Fatal("source-ordered hit after kill must retain uncertainty marker")
	}
	// Another installation in this guild must not change this response.
	var otherServerID int64
	if err:=w.a.DB.Pool.QueryRow(ctx,`
	INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
	VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
		w.guildID,fmt.Sprintf("case-session-other-%d",time.Now().UnixNano()),w.f.OrgID).Scan(&otherServerID);err!=nil{t.Fatal(err)}
	extra:=caseSessionFixture(w.guildID,otherServerID,600,source,"PLAYER_RESPAWN")
	if err:=repo.RecordCaseEvidence(ctx,extra);err!=nil{t.Fatal(err)}
	rr=w.call(w.a.handleAntiCheatSessions,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	out=decodeBody[caseSessionPage](t,rr)
	if out.Sources[0].ObservationCount!=4||out.LoginObservations.RepeatedConnectWindows!=0{t.Fatal("cross-installation session record leaked")}
	// A user lacking authorized scope cannot read player positions.
	stranger:=syncUser(t,w.a,fmt.Sprintf("case-session-stranger-%d",time.Now().UnixNano()),"Stranger")
	denied:=w.call(w.a.handleAntiCheatSessions,http.MethodGet,path,stranger.DiscordUserID,nil,nil)
	if denied.Code!=http.StatusForbidden{t.Fatalf("unauthorized read: %d %s",denied.Code,denied.Body.String())}
}
func TestCASESessionAPIValidationAndBoundedPagination(t *testing.T){
	w:=newClientAdminWorld(t)
	repo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	for i:=int64(1);i<=3;i++{
		x:=caseSessionFixture(w.guildID,w.serverID,i*100,"dayzps/config/window.ADM","PLAYER_CONNECT")
		if err:=repo.RecordCaseEvidence(context.Background(),x);err!=nil{t.Fatal(err)}
	}
	var playerID int64
	if err:=w.a.DB.Pool.QueryRow(context.Background(),
		"SELECT id FROM players WHERE guild_id=$1 AND dayz_player_id='case-session-victim'",w.guildID).Scan(&playerID);err!=nil{t.Fatal(err)}
	path:=fmt.Sprintf("%s?playerId=%d&limit=2",w.path("/anti-cheat/sessions"),playerID)
	rr:=w.call(w.a.handleAntiCheatSessions,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusOK{t.Fatalf("page read: %d %s",rr.Code,rr.Body.String())}
	out:=decodeBody[caseSessionPage](t,rr)
	if !out.WindowTruncated||out.NextCursor==nil||out.Sources[0].ObservationCount!=2{
		t.Fatalf("window limit not disclosed: %+v",out)
	}
	if !out.LoginObservations.WindowTruncated||out.LoginObservations.Status!="OBSERVATION_ONLY"||out.LoginObservations.DetectorsEnabled {t.Fatalf("login page overclaimed: %+v",out.LoginObservations)}
	invalid:=w.call(w.a.handleAntiCheatSessions,http.MethodGet,
		w.path("/anti-cheat/sessions?playerId=garbage"),w.f.OwnerDiscordID,nil,nil)
	if invalid.Code!=http.StatusBadRequest{t.Fatalf("invalid player ID accepted: %d",invalid.Code)}
	invalid=w.call(w.a.handleAntiCheatSessions,http.MethodGet,
		fmt.Sprintf("%s?playerId=%d&limit=9999",w.path("/anti-cheat/sessions"),playerID),
		w.f.OwnerDiscordID,nil,nil)
	if invalid.Code!=http.StatusBadRequest{t.Fatalf("unbounded scan accepted: %d",invalid.Code)}
}

func TestCASESessionLoginShadowUsesLearnedClockAndFailsClosed(t *testing.T){
	w:=newClientAdminWorld(t)
	ctx:=context.Background()
	repo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	source:="dayzps/config/DayZServer_PS4_x64_2026-09-30_22-00-00.ADM"
	kinds:=[]string{"PLAYER_CONNECT","PLAYER_DISCONNECT","PLAYER_CONNECT","PLAYER_DISCONNECT","PLAYER_CONNECT"}
	for i,kind:=range kinds{
		x:=caseSessionFixture(w.guildID,w.serverID,int64(i+1)*100,source,kind)
		x.ADMClock=fmt.Sprintf("22:%02d:00",10+i)
		if err:=repo.RecordCaseEvidence(ctx,x);err!=nil{t.Fatal(err)}
	}
	var playerID int64
	if err:=w.a.DB.Pool.QueryRow(ctx,
		"SELECT id FROM players WHERE guild_id=$1 AND dayz_player_id='case-session-victim'",
		w.guildID).Scan(&playerID);err!=nil{t.Fatal(err)}
	path:=fmt.Sprintf("%s?playerId=%d",w.path("/anti-cheat/sessions"),playerID)
	read:=func()caseSessionPage{
		rr:=w.call(w.a.handleAntiCheatSessions,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
		if rr.Code!=http.StatusOK{t.Fatalf("sessions: %d %s",rr.Code,rr.Body.String())}
		return decodeBody[caseSessionPage](t,rr)
	}
	// No learned offset: nothing trusted.
	if sh:=read().LoginShadow;sh.TrustedEvents!=0||sh.UntrustedEvents["UTC_OFFSET_UNKNOWN"]!=5||len(sh.Result.Findings)!=0{
		t.Fatalf("time trusted without learned offset: %+v",sh)
	}
	if _,err:=w.a.DB.Pool.Exec(ctx,`INSERT INTO live_sync_server_clock(server_id,guild_id,utc_offset_minutes,learned_from)
		VALUES($1,$2,-240,'restart.log test')`,w.serverID,w.guildID);err!=nil{t.Fatal(err)}
	sh:=read().LoginShadow
	// Times are now trusted, but this test world has no live ADM worker, so
	// the detector reports missing telemetry and draws no conclusion.
	if sh.TrustedEvents!=5||len(sh.UntrustedEvents)!=0||sh.Result.Status!="INSUFFICIENT_EVIDENCE"||
		len(sh.Result.Findings)!=0||sh.Result.CanNotify||sh.Result.Enforcement!="DISABLED"{
		t.Fatalf("learned clock shadow: %+v",sh)
	}
}
