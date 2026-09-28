//go:build integration

package app

import (
 "context"
 "fmt"
 "net/http"
 "testing"
)

func TestSecurityMarketplaceCatalogUsesVerifiedPlayerAndInstallationScope(t *testing.T) {
 w:=newFactionWorld(t)
 player:=w.players[0]
 w.linkPlayer(w.a1,player,"Marketplace Player")
 path:=func(f installationFixture,suffix string)string{
  return fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace%s",f.OrgID,f.InstallationID,suffix)
 }
 var before,after int
 if err:=w.a.DB.Pool.QueryRow(context.Background(),"SELECT COUNT(*) FROM point_transactions").Scan(&before);err!=nil {t.Fatal(err)}
 response:=w.expect(w.do(http.MethodGet,path(w.a1,"/catalog"),player,nil),http.StatusOK,"verified player catalog").JSON(t)
 if int64(response["installationId"].(float64))!=w.a1.InstallationID||response["gameServerId"]==nil {t.Fatalf("incorrect installation scope: %+v",response)}
 items:=response["items"].([]any)
 if len(items)!=7 {t.Fatalf("unexpected security-service count: %d",len(items))}
 for _,entry:=range items {
  item:=entry.(map[string]any)
  if item["purchasable"]!=false||item["status"]!="UNSUPPORTED" {t.Fatalf("unverified purchase exposed: %+v",item)}
 }
 w.expect(w.do(http.MethodGet,path(w.a1,"/admin/catalog"),w.admin,nil),http.StatusOK,"owner/admin catalog")
 w.expect(w.do(http.MethodGet,path(w.a1,"/admin/catalog"),w.member,nil),http.StatusForbidden,"member cannot administer")
 w.expect(w.do(http.MethodGet,path(w.b1,"/catalog"),player,nil),http.StatusConflict,"foreign guild has no verified player link")
 w.expect(w.do(http.MethodGet,path(w.a1,"/catalog"),w.players[1],nil),http.StatusConflict,"unlinked player")
 if err:=w.a.DB.Pool.QueryRow(context.Background(),"SELECT COUNT(*) FROM point_transactions").Scan(&after);err!=nil {t.Fatal(err)}
 if before!=after {t.Fatalf("read-only catalog wrote to credit ledger: %d -> %d",before,after)}
}
