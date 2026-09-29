package casereview

import (
 "strings"
 "testing"
 "time"
)

func fixture() SyntheticInput {
 return SyntheticInput{ID:"SYNTHETIC-001",Scope:Scope{GuildID:1,InstallationID:2,ServerID:3},
  DetectorID:"DEMO-ONLY",DetectorVersion:"0.0.0",EvidenceIDs:[]int64{17,19},
  SourceQualityRef:"fixture-ref-01",At:time.Date(2026,9,27,12,0,0,0,time.UTC)}
}
func staff() Reviewer {return Reviewer{ID:"offline-reviewer",Authorized:true}}
func action(id string,to Status,step time.Duration) Action {
 return Action{ID:id,To:to,Note:"Synthetic review only",At:fixture().At.Add(step)}
}
func TestSyntheticReviewTransitionsAndAudit(t *testing.T) {
 c,err:=NewSynthetic(fixture());if err!=nil{t.Fatal(err)}
 for _,a:=range []Action{action("a",PendingReview,time.Second),action("b",Reviewed,2*time.Second),action("c",Resolved,3*time.Second)} {
  applied,err:=c.ApplySynthetic(staff(),a);if err!=nil||!applied {t.Fatalf("action %+v: %v %v",a,applied,err)}
 }
 s:=c.Snapshot()
 if !s.Synthetic||s.Status!=Resolved||len(s.History)!=3||s.History[0].From!=Draft||s.History[2].To!=Resolved {t.Fatalf("invalid audit: %+v",s)}
 if applied,err:=c.ApplySynthetic(staff(),action("c",Resolved,3*time.Second));err!=nil||applied {t.Fatalf("replay not idempotent: %v %v",applied,err)}
 if applied,err:=c.ApplySynthetic(staff(),action("c",Dismissed,3*time.Second));err==nil||applied {t.Fatal("colliding id accepted")}
 if len(c.Snapshot().History)!=3 {t.Fatal("replay mutated history")}
}

func TestInvalidAndUnauthorizedActionsNeverMutate(t *testing.T) {
 c,err:=NewSynthetic(fixture());if err!=nil{t.Fatal(err)}
 for _,tc:=range []struct{who Reviewer;a Action}{
  {Reviewer{ID:"visitor"},action("a",PendingReview,time.Second)},
  {Reviewer{Authorized:true},action("a",PendingReview,time.Second)},
  {staff(),action("a",Reviewed,time.Second)},
  {staff(),action("a",Resolved,time.Second)},
  {staff(),action("",PendingReview,time.Second)},
  {staff(),Action{ID:"a",To:PendingReview,Note:"hi @everyone",At:fixture().At.Add(time.Second)}},
  {staff(),Action{ID:"a",To:PendingReview,Note:strings.Repeat("x",501),At:fixture().At.Add(time.Second)}},
  {staff(),action("a",PendingReview,-time.Second)},
 } {
  if ok,e:=c.ApplySynthetic(tc.who,tc.a);e==nil||ok {t.Fatalf("unsafe action admitted: %+v",tc)}
  if s:=c.Snapshot();s.Status!=Draft||len(s.History)!=0{t.Fatalf("rejected mutation changed case: %+v",s)}
 }
 if _,err:=c.ApplySynthetic(staff(),action("p",PendingReview,time.Second));err!=nil{t.Fatal(err)}
 if _,err:=c.ApplySynthetic(staff(),action("d",Dismissed,2*time.Second));err!=nil{t.Fatal(err)}
 if ok,err:=c.ApplySynthetic(staff(),action("r",Reviewed,3*time.Second));err==nil||ok{t.Fatal("dismissal is terminal")}
}

func TestFixtureProvenanceAndSnapshotIsolation(t *testing.T) {
 for _,change:=range []func(*SyntheticInput){
  func(x *SyntheticInput){x.ID=""},func(x *SyntheticInput){x.Scope.InstallationID=0},
  func(x *SyntheticInput){x.Scope.ServerID=0},func(x *SyntheticInput){x.EvidenceIDs=[]int64{1,1}},
  func(x *SyntheticInput){x.EvidenceIDs=[]int64{-1}},func(x *SyntheticInput){x.EvidenceIDs=nil},
  func(x *SyntheticInput){x.SourceQualityRef="../raw.ADM"},
  func(x *SyntheticInput){x.At=time.Time{}},
 } {
  x:=fixture();change(&x);if _,err:=NewSynthetic(x);err==nil{t.Fatalf("accepted unsafe input %+v",x)}
 }
 input:=fixture();c,err:=NewSynthetic(input);if err!=nil{t.Fatal(err)}
 input.EvidenceIDs[0]=999
 snapshot:=c.Snapshot()
 snapshot.EvidenceIDs[0]=999
 if c.Snapshot().EvidenceIDs[0]!=17{t.Fatal("evidence references mutated through input or snapshot")}
 c.ApplySynthetic(staff(),action("a",PendingReview,time.Second))
 snapshot=c.Snapshot();snapshot.History[0].Note="mutated"
 if c.Snapshot().History[0].Note=="mutated"{t.Fatal("audit exposed mutable slice")}
}
