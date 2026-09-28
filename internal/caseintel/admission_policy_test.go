package caseintel

import (
 "strings"
 "testing"
 "time"
)

func TestCaseAdmissionNeverAcceptsCurrentBlockedDetector(t *testing.T) {
 now:=time.Date(2026,9,28,1,0,0,0,time.UTC)
 base:=CaseAdmissionCandidate{
  GuildID:1,InstallationID:2,ServerID:3,
  DetectorID:"CASE-MOV-001",DetectorVersion:"0.1.0",
  EvaluationStatus:"ELIGIBLE_SHADOW",
  SelectedSource:"source-current",AcceptedSource:"source-current",LatestRetainedSource:"source-current",
  CapturedAt:now,LatestRetainedAt:now.Add(-time.Minute),SourceChangedAt:now.Add(-time.Hour),
  Evidence:[]AdmissibilitySample{{
   EvidenceID:12,SourceID:"source-current",SourceEndOffset:120,
   LineSHA256:strings.Repeat("a",64),EventType:"PLAYER_HIT",ADMClock:"12:00:00",
  }},
 }
 check:=func(name string,in CaseAdmissionCandidate,want string){
  t.Helper();t.Run(name,func(t *testing.T){
   got:=AssessCaseAdmission(in)
   if got.Status!="BLOCKED"||got.EvidenceFingerprint!=""{
    t.Fatalf("current registry improperly admitted: %+v",got)
   }
   found:=false
   for _,b:=range got.Blockers{if b==want{found=true}}
   if !found{t.Fatalf("missing %s: %+v",want,got)}
  })
 }
 check("blocked registry even with caller claims",base,"DETECTOR_NOT_VALIDATED")
 unknown:=base;unknown.DetectorID="CASE-FAKE-001"
 check("unknown detector",unknown,"DETECTOR_VERSION_NOT_REGISTERED")
 wrongVersion:=base;wrongVersion.DetectorVersion="999"
 check("unregistered version",wrongVersion,"DETECTOR_VERSION_NOT_REGISTERED")
 invalidScope:=base;invalidScope.InstallationID=0
 check("invalid installation",invalidScope,"INVALID_INSTALLATION_SCOPE")
 source:=base;source.LatestRetainedSource="previous-boot"
 check("historical retained source",source,"CURRENT_SOURCE_NOT_VERIFIED")
 accepted:=base;accepted.AcceptedSource="previous-boot"
 check("selected not accepted",accepted,"CURRENT_SOURCE_NOT_VERIFIED")
 old:=base;old.LatestRetainedAt=now.Add(-2*time.Hour)
 check("retained event predates source change",old,"CURRENT_EVIDENCE_TIME_NOT_VERIFIED")
 future:=base;future.LatestRetainedAt=now.Add(time.Minute)
 check("future ingestion",future,"CURRENT_EVIDENCE_TIME_NOT_VERIFIED")
 truncated:=base;truncated.WindowTruncated=true
 check("bounded source window edge",truncated,"EVIDENCE_WINDOW_INCOMPLETE")
 empty:=base;empty.Evidence=nil
 check("no linked evidence",empty,"EVIDENCE_WINDOW_INCOMPLETE")
 foreign:=base;foreign.Evidence=[]AdmissibilitySample{base.Evidence[0]}
 foreign.Evidence[0].SourceID="previous-boot"
 check("foreign source evidence",foreign,"EVIDENCE_SOURCE_ADDRESS_UNVERIFIED")
 malformed:=base;malformed.Evidence=[]AdmissibilitySample{base.Evidence[0]}
 malformed.Evidence[0].LineSHA256="not-a-sha"
 check("malformed hash",malformed,"EVIDENCE_SOURCE_ADDRESS_UNVERIFIED")
 duplicate:=base;duplicate.Evidence=[]AdmissibilitySample{base.Evidence[0],base.Evidence[0]}
 check("duplicate evidence id",duplicate,"INVALID_OR_DUPLICATE_EVIDENCE_ID")
 collided:=base;collided.Evidence=[]AdmissibilitySample{base.Evidence[0],base.Evidence[0]}
 collided.Evidence[1].EvidenceID=13
 collided.Evidence[1].LineSHA256=strings.Repeat("b",64)
 check("same source offset different hash",collided,"EVIDENCE_SOURCE_OFFSET_HASH_COLLISION")
 repeat:=base;repeat.Evidence=[]AdmissibilitySample{base.Evidence[0],base.Evidence[0]}
 repeat.Evidence[1].EvidenceID=13
 check("same source address duplicate",repeat,"REPEATED_SOURCE_ADDRESS")
 blockedEval:=base;blockedEval.EvaluationStatus="BLOCKED"
 check("blocked evaluation",blockedEval,"SHADOW_EVALUATION_NOT_ELIGIBLE")
 // Returned registry definitions are copies, never mutable release approvals.
 defs:=Registry();defs[0].Mode="VALIDATED_SHADOW"
 check("mutated caller registry copy cannot approve",base,"DETECTOR_NOT_VALIDATED")
}
