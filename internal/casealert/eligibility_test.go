package casealert

import (
 "strings"
 "testing"
)

func valid() Request {
 s:=Scope{GuildID:7,InstallationID:11,ServerID:19}
 return Request{Scope:s,EvidenceScope:s,RouteScope:s,FindingID:"opaque-reviewed-fixture",
  EventVersion:1,DetectorID:"TEST-SUPPORTED",EvidenceRef:"opaque-evidence-fixture",
  ReviewState:"REVIEWED",SourceMatched:true,QualityAccepted:true,DetectorValidated:true,
  ReviewerAuthorized:true,EvidenceAccessible:true,StaffAlertsEnabled:true,RoutePrivate:true,
  RouteReachable:true,EnforcementDisabled:true}
}
func has(p Plan,s string)bool{for _,v:=range p.Blockers{if v==s{return true}};return false}
func TestOfflineDeliveryRequiresEveryGate(t *testing.T){
 all:=valid()
 p:=Evaluate(all)
 if !p.Eligible||len(p.Key)!=64||len(p.Blockers)!=0{t.Fatalf("valid synthetic plan: %+v",p)}
 if again:=Evaluate(all);again.Key!=p.Key{t.Fatal("same event version has unstable dedupe key")}
 v2:=all;v2.EventVersion=2
 if Evaluate(v2).Key==p.Key{t.Fatal("new reviewed event version must have distinct delivery key")}
 foreign:=all;foreign.Scope.InstallationID=12;foreign.EvidenceScope=foreign.Scope;foreign.RouteScope=foreign.Scope
 if Evaluate(foreign).Key==p.Key{t.Fatal("two installations shared delivery identity")}
 for _,tc:=range []struct{reason string; mutate func(*Request)}{
  {"INVALID_INSTALLATION_SCOPE",func(x *Request){x.Scope.ServerID=0}},
  {"EVIDENCE_SCOPE_MISMATCH",func(x *Request){x.EvidenceScope.InstallationID=12}},
  {"PRIVATE_ROUTE_SCOPE_MISMATCH",func(x *Request){x.RouteScope.GuildID=8}},
  {"MISSING_IMMUTABLE_FINDING_REFERENCE",func(x *Request){x.FindingID=""}},
  {"MISSING_IMMUTABLE_FINDING_REFERENCE",func(x *Request){x.EventVersion=0}},
  {"INVALID_EVIDENCE_REFERENCE",func(x *Request){x.EvidenceRef="../private.ADM"}},
  {"STAFF_REVIEW_REQUIRED",func(x *Request){x.ReviewState="PENDING_REVIEW"}},
  {"CURRENT_SOURCE_NOT_VERIFIED",func(x *Request){x.SourceMatched=false}},
  {"EVIDENCE_QUALITY_NOT_ACCEPTED",func(x *Request){x.QualityAccepted=false}},
  {"DETECTOR_NOT_VALIDATED",func(x *Request){x.DetectorValidated=false}},
  {"REVIEWER_NOT_AUTHORIZED",func(x *Request){x.ReviewerAuthorized=false}},
  {"EVIDENCE_LINK_NOT_AUTHORIZED",func(x *Request){x.EvidenceAccessible=false}},
  {"STAFF_DELIVERY_NOT_ENABLED",func(x *Request){x.StaffAlertsEnabled=false}},
  {"PRIVATE_ROUTE_UNAVAILABLE",func(x *Request){x.RoutePrivate=false}},
  {"PRIVATE_ROUTE_UNAVAILABLE",func(x *Request){x.RouteReachable=false}},
  {"ENFORCEMENT_MUST_REMAIN_DISABLED",func(x *Request){x.EnforcementDisabled=false}},
 }{
  x:=all;tc.mutate(&x)
  got:=Evaluate(x)
  if got.Eligible||got.Key!=""||!has(got,tc.reason) {t.Fatalf("failed to block %s: %+v",tc.reason,got)}
 }
}
func TestOfflinePlanDoesNotExposeIdentityOrInventReadiness(t *testing.T){
 p:=Evaluate(Request{})
 if p.Eligible||p.Key!=""||!has(p,"CURRENT_SOURCE_NOT_VERIFIED")||!has(p,"DETECTOR_NOT_VALIDATED") {t.Fatalf("missing inputs must fail closed: %+v",p)}
 original:=valid();original.FindingID="opaque-other"
 p=Evaluate(original)
 if !p.Eligible||strings.Contains(p.Key,original.FindingID){t.Fatal("delivery key exposed case ID or failed synthetic fixture")}
}
