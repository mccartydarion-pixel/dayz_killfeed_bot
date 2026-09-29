//go:build integration

package app

import (
 "net/http"
 "testing"

 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCaseDetectorSensitivityOwnerConfigurationIsInert(t *testing.T){
 w:=newClientAdminWorld(t)
 admin:=zoneActor(t,w,"case-settings-admin")
 w.mapRole(admin,"case-settings-admin-role","ADMINISTRATOR")
 listPath:=w.path("/case/detector-settings")
 denied:=w.call(w.a.handleCaseListDetectorSettings,http.MethodGet,listPath,admin,nil,nil)
 if denied.Code!=http.StatusForbidden{t.Fatalf("non-owner read preferences: %d",denied.Code)}
 listed:=w.call(w.a.handleCaseListDetectorSettings,http.MethodGet,listPath,w.f.OwnerDiscordID,nil,nil)
 if listed.Code!=http.StatusOK{t.Fatalf("defaults: %d %s",listed.Code,listed.Body.String())}
 initial:=decodeBody[struct{
  Items []repository.CaseDetectorSetting `json:"items"`
  Operational bool `json:"operational"`
 }](t,listed)
 if len(initial.Items)!=8||initial.Operational{t.Fatalf("unsafe defaults: %+v",initial)}
 path:=w.path("/case/detector-settings/x")
 pv:=map[string]string{"moduleID":"CASE-LOGIN-001"}
 denied=w.call(w.a.handleCaseSetDetectorSetting,http.MethodPut,path,admin,
  caseSetDetectorSettingRequest{Sensitivity:caseintel.SensitivityStrict},pv)
 if denied.Code!=http.StatusForbidden{t.Fatalf("non-owner saved preference: %d",denied.Code)}
 saved:=w.call(w.a.handleCaseSetDetectorSetting,http.MethodPut,path,w.f.OwnerDiscordID,
  caseSetDetectorSettingRequest{Sensitivity:caseintel.SensitivityStrict},pv)
 if saved.Code!=http.StatusOK{t.Fatalf("save: %d %s",saved.Code,saved.Body.String())}
 result:=decodeBody[struct{
  Setting repository.CaseDetectorSetting `json:"setting"`
  Operational bool `json:"operational"`
 }](t,saved)
 if result.Operational||result.Setting.ModuleID!="CASE-LOGIN-001"||
  result.Setting.Sensitivity!=caseintel.SensitivityStrict||result.Setting.Revision!=1 {
  t.Fatalf("unsafe save: %+v",result)
 }
 bad:=w.call(w.a.handleCaseSetDetectorSetting,http.MethodPut,path,w.f.OwnerDiscordID,
  map[string]any{"sensitivity":"STRICT","enabled":true},pv)
 if bad.Code!=http.StatusBadRequest{t.Fatalf("enablement field accepted: %d",bad.Code)}
 pv["moduleID"]="CASE-MOV-001"
 bad=w.call(w.a.handleCaseSetDetectorSetting,http.MethodPut,path,w.f.OwnerDiscordID,
  caseSetDetectorSettingRequest{Sensitivity:caseintel.SensitivityStrict},pv)
 if bad.Code!=http.StatusBadRequest{t.Fatalf("ninth detector accepted: %d",bad.Code)}
 pv["moduleID"]="CASE-LOGIN-001"
 bad=w.call(w.a.handleCaseSetDetectorSetting,http.MethodPut,path,w.f.OwnerDiscordID,
  caseSetDetectorSettingRequest{Sensitivity:caseintel.Sensitivity("ENABLED")},pv)
 if bad.Code!=http.StatusBadRequest{t.Fatalf("invalid mode accepted: %d",bad.Code)}
 listed=w.call(w.a.handleCaseListDetectorSettings,http.MethodGet,listPath,w.f.OwnerDiscordID,nil,nil)
 after:=decodeBody[struct{Items []repository.CaseDetectorSetting `json:"items"`}](t,listed)
 if len(after.Items)!=8{t.Fatalf("changed catalog size: %d",len(after.Items))}
 for _,s:=range after.Items{
  if s.ModuleID=="CASE-LOGIN-001"{
   if !s.Configured||s.Sensitivity!=caseintel.SensitivityStrict {t.Fatalf("not saved: %+v",s)}
  }else if s.Configured||s.Sensitivity!=caseintel.SensitivityBalanced {t.Fatalf("other module changed: %+v",s)}
 }
}
