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

func caseHitInput(guildID,serverID,offset int64, source,hash string) repository.CaseEvidenceInput {
	x,z,alt,damage,hp,distance:=100.0,200.0,5.0,28.5,55.0,123.4
	return repository.CaseEvidenceInput{
		GuildID:guildID,ServerID:serverID,SourceID:source,SourceEndOffset:offset,
		LineSHA256:hash,EventType:"PLAYER_HIT",ADMClock:"17:20:01",
		Actor:repository.CaseEvidencePerson{DayZID:"evidence-attacker",Name:"Attacker",X:&x,Z:&z,Altitude:&alt},
		Target:repository.CaseEvidencePerson{DayZID:"evidence-victim",Name:"Victim"},
		Weapon:"M4-A1",Ammo:"Bullet_556x45",HitZone:"Torso",Damage:&damage,HP:&hp,DistanceMeters:&distance,
	}
}

func TestCASEEvidenceDurableReplayAndTenantIsolation(t *testing.T) {
	w:=newClientAdminWorld(t)
	repo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	ctx:=context.Background()
	source:="dayzps/config/DayZServer_PS4_x64_2026-09-23.ADM"
	hash:=fmt.Sprintf("%064x",1234)
	event:=caseHitInput(w.guildID,w.serverID,100,source,hash)
	if err:=repo.RecordCaseEvidence(ctx,event);err!=nil {t.Fatal(err)}
	if err:=repo.RecordCaseEvidence(ctx,event);err!=nil {t.Fatalf("exact replay should be idempotent: %v",err)}
	second:=event
	second.SourceEndOffset=300
	if err:=repo.RecordCaseEvidence(ctx,second);err!=nil {t.Fatal(err)}
	changed:=event
	changed.LineSHA256=fmt.Sprintf("%064x",5678)
	if err:=repo.RecordCaseEvidence(ctx,changed);err==nil {t.Fatal("same source address with different content MUST fail")}
	items,err:=repo.ListCaseEvidence(ctx,w.guildID,w.serverID,nil,nil,50)
	if err!=nil {t.Fatal(err)}
	if len(items)!=2 || items[0].SourceEndOffset!=300 || items[1].SourceEndOffset!=100 {
		t.Fatalf("replays or separate identical hits were collapsed: %+v",items)
	}
	if items[0].ActorPlayerID==nil || items[0].TargetPlayerID==nil || items[0].ActorName!="Attacker" || items[0].Damage==nil || *items[0].Damage!=28.5 {
		t.Fatalf("evidence fields not retained: %+v",items[0])
	}
	if items[0].SourceRef=="" || items[0].SourceRef==source {
		t.Fatal("API must expose a pseudonym, not Nitrado source path")
	}
	before:=items[0].ID
	paged,err:=repo.ListCaseEvidence(ctx,w.guildID,w.serverID,nil,&before,1)
	if err!=nil || len(paged)!=1 || paged[0].ID!=items[1].ID {
		t.Fatalf("cursor paging wrong: %+v / %v",paged,err)
	}
	// Same guild, different server: query must not cross installation boundary.
	var otherID int64
	if err:=w.a.DB.Pool.QueryRow(ctx, `
INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
		w.guildID,fmt.Sprintf("case-evidence-other-%d",time.Now().UnixNano()),w.f.OrgID).Scan(&otherID);err!=nil {t.Fatal(err)}
	other:=caseHitInput(w.guildID,otherID,100,source,hash)
	if err:=repo.RecordCaseEvidence(ctx,other);err!=nil {t.Fatal(err)}
	count,err:=repo.CaseHitCount(ctx,w.guildID,w.serverID,time.Now().Add(-time.Hour),time.Now().Add(time.Hour))
	if err!=nil || count!=2 {t.Fatalf("count must exclude other server: %d / %v",count,err)}
	otherList,err:=repo.ListCaseEvidence(ctx,w.guildID,otherID,nil,nil,50)
	if err!=nil || len(otherList)!=1 {t.Fatalf("other server data wrong: %+v / %v",otherList,err)}
}

func TestCASEEvidenceAPIRequiresLocationCapabilityAndDoesNotFabricateVerdicts(t *testing.T) {
	w:=newClientAdminWorld(t)
	repo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	if err:=repo.RecordCaseEvidence(context.Background(),caseHitInput(w.guildID,w.serverID,250,
		"dayzps/config/a.ADM",fmt.Sprintf("%064x",321)));err!=nil {t.Fatal(err)}
	rr:=w.call(w.a.handleAntiCheatEvidence,http.MethodGet,w.path("/anti-cheat/evidence"),
		w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusOK {t.Fatalf("owner read failed %d: %s",rr.Code,rr.Body.String())}
	out:=decodeBody[caseEvidencePage](t,rr)
	if len(out.Items)!=1 || out.TimeBasis!="ADM_CLOCK_ONLY_WITH_INGESTION_ORDER" ||
		out.DetectorsEnabled || out.Enforcement!="DISABLED" {
		t.Fatalf("incorrect C.A.S.E. evidence contract: %+v",out)
	}
	stranger:=syncUser(t,w.a,fmt.Sprintf("case-evidence-stranger-%d",time.Now().UnixNano()),"Stranger")
	denied:=w.call(w.a.handleAntiCheatEvidence,http.MethodGet,w.path("/anti-cheat/evidence"),
		stranger.DiscordUserID,nil,nil)
	if denied.Code!=http.StatusForbidden {t.Fatalf("unprivileged read got %d: %s",denied.Code,denied.Body.String())}
	t.Setenv("CASE_EVIDENCE_ENABLED","true")
	overview:=w.call(w.a.handleAntiCheatOverview,http.MethodGet,w.path("/anti-cheat/overview"),
		w.f.OwnerDiscordID,nil,nil)
	if overview.Code!=http.StatusOK {t.Fatalf("overview failed %d: %s",overview.Code,overview.Body.String())}
	summary:=decodeBody[caseObservationResponse](t,overview)
	if !summary.EvidenceConfigured || summary.Telemetry.HitEvents24h==nil || *summary.Telemetry.HitEvents24h!=1 || len(summary.Alerts)!=0 || len(summary.Cases)!=0 {
		t.Fatalf("evidence status/coverage wrong: %+v",summary)
	}
}
