//go:build integration

package app

import (
 "context"
 "net/http"
 "strconv"
 "testing"

 "github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCaseBaseDraftAdminFlowIsOwnerScopedAndInert(t *testing.T){
 w:=newClientAdminWorld(t)
 owner:=w.seedPlayer("Base Owner")
 guest:=w.seedPlayer("Guest")
 req:=caseCreateBaseDraftRequest{
  OwnerPlayerID:owner,MapKey:"chernarusplus",Name:"North Base",
  CenterX:100,CenterZ:200,Radius:50,
 }
 admin:=zoneActor(t,w,"case-base-admin")
 w.mapRole(admin,"case-base-admin-role","ADMINISTRATOR")
 denied:=w.call(w.a.handleCaseCreateBaseDraft,http.MethodPost,w.path("/case/bases"),admin,req,nil)
 if denied.Code!=http.StatusForbidden{t.Fatalf("non-owner created a base: %d %s",denied.Code,denied.Body.String())}
 created:=w.call(w.a.handleCaseCreateBaseDraft,http.MethodPost,w.path("/case/bases"),w.f.OwnerDiscordID,req,nil)
 if created.Code!=http.StatusCreated{t.Fatalf("draft creation: %d %s",created.Code,created.Body.String())}
 out:=decodeBody[struct{
  Base repository.CaseRegisteredBase `json:"base"`
  Operational bool `json:"operational"`
 }](t,created)
 if out.Base.ID<=0||out.Base.State!="DRAFT"||out.Operational||
  out.Base.InstallationID!=w.f.InstallationID||out.Base.ServerID!=w.serverID{
  t.Fatalf("unsafe draft response: %+v",out)
 }
 bad:=w.call(w.a.handleCaseCreateBaseDraft,http.MethodPost,w.path("/case/bases"),
  w.f.OwnerDiscordID,map[string]any{"ownerPlayerId":owner,"mapKey":"chernarusplus",
  "name":"Forged","centerX":100,"centerZ":200,"radius":50,"state":"REVIEWED"},nil)
 if bad.Code!=http.StatusBadRequest{t.Fatalf("client set review state: %d %s",bad.Code,bad.Body.String())}
 listed:=w.call(w.a.handleCaseListBases,http.MethodGet,w.path("/case/bases"),w.f.OwnerDiscordID,nil,nil)
 if listed.Code!=http.StatusOK{t.Fatalf("list: %d %s",listed.Code,listed.Body.String())}
 list:=decodeBody[struct{
  Items []repository.CaseRegisteredBase `json:"items"`
  Operational bool `json:"operational"`
 }](t,listed)
 if len(list.Items)!=1||list.Items[0].ID!=out.Base.ID||list.Operational{t.Fatalf("unsafe list: %+v",list)}
 path:=w.path("/case/bases/x/grants")
 pv:=map[string]string{"baseID":strconv.FormatInt(out.Base.ID,10)}
 grant:=w.call(w.a.handleCaseAddBaseGrant,http.MethodPost,path,w.f.OwnerDiscordID,
  caseAddGrantRequest{PlayerID:&guest},pv)
 if grant.Code!=http.StatusCreated{t.Fatalf("grant: %d %s",grant.Code,grant.Body.String())}
 got:=decodeBody[struct{
  Grant repository.CaseBaseAuthorization `json:"grant"`
  Operational bool `json:"operational"`
 }](t,grant)
 if got.Grant.ID<=0||got.Grant.PlayerID==nil||*got.Grant.PlayerID!=guest||
  got.Operational{t.Fatalf("unsafe grant: %+v",got)}
 grants:=w.call(w.a.handleCaseListBaseGrants,http.MethodGet,path,w.f.OwnerDiscordID,nil,pv)
 if grants.Code!=http.StatusOK{t.Fatalf("list grants: %d %s",grants.Code,grants.Body.String())}
 grantList:=decodeBody[struct{Items []repository.CaseBaseAuthorization `json:"items"`}](t,grants)
 if len(grantList.Items)!=1||grantList.Items[0].ID!=got.Grant.ID{t.Fatalf("grant readback: %+v",grantList)}
 invalid:=w.call(w.a.handleCaseAddBaseGrant,http.MethodPost,path,w.f.OwnerDiscordID,
  caseAddGrantRequest{PlayerID:&guest,FactionID:&guest},pv)
 if invalid.Code!=http.StatusBadRequest{t.Fatalf("ambiguous grant accepted: %d",invalid.Code)}
 denied= w.call(w.a.handleCaseListBaseGrants,http.MethodGet,path,admin,nil,pv)
 if denied.Code!=http.StatusForbidden{t.Fatalf("non-owner read grants: %d",denied.Code)}
 var state string
 if err:=w.a.DB.Pool.QueryRow(context.Background(),`SELECT state FROM case_registered_bases WHERE id=$1`,out.Base.ID).Scan(&state);err!=nil||state!="DRAFT"{
  t.Fatalf("draft was promoted: %q %v",state,err)
 }
}
