package app

import (
 "testing"
 "time"
)

func TestContinuityAssessmentDoesNotConfuseNoEventsWithNoGameplay(t *testing.T) {
 source:=caseSourceRef("dayzps/config/boot.ADM")
 when:=time.Date(2026,9,25,0,54,27,0,time.UTC)
 records:=[]caseContinuitySource{{SourceRef:*source,RecordedLines:7,HitLines:2,KillLines:1,RespawnLines:1,LatestIngestedAt:when}}
 q:=caseContinuityAssessment(source,records,true,true,true)
 if q.CurrentSourceEvidenceStatus!="RETAINED_EVENTS_OBSERVED"||
  q.CurrentSourceRecordedLines!=7||q.CurrentSourceHitLines!=2||q.CurrentSourceKillLines!=1||
  q.LatestCurrentSourceIngestedAt==nil||!q.LatestCurrentSourceIngestedAt.Equal(when)||
  q.CheckpointContinuity!="CHECKPOINT_REPORTED_NOT_FULL_COVERAGE"||
  q.TrustedElapsedTime||q.DetectorsEnabled||q.Enforcement!="DISABLED" {t.Fatalf("unsafe continuity claim: %+v",q)}
 q=caseContinuityAssessment(caseSourceRef("different.ADM"),records,true,true,true)
 if q.CurrentSourceEvidenceStatus!="NO_RETAINED_EVENTS_IN_RETURNED_SOURCES"||
  q.CurrentSourceRecordedLines!=0||q.LatestCurrentSourceIngestedAt!=nil {t.Fatalf("unrelated source incorrectly claimed: %+v",q)}
 q=caseContinuityAssessment(source,records,false,true,false)
 if q.CurrentSourceEvidenceStatus!="UNKNOWN"||q.CheckpointContinuity!="NOT_PROVEN" {t.Fatalf("absent worker cannot prove continuity: %+v",q)}
 q=caseContinuityAssessment(source,records,true,false,true)
 if q.CurrentSourceEvidenceStatus!="UNKNOWN" {t.Fatal("disabled collector is not a clean zero")}
}
