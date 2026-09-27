package repository

import (
 "strings"
 "testing"
 "time"
)

func validSyntheticReview() SyntheticReviewInput {
 return SyntheticReviewInput{
  FixtureOnly:true,CallerCapabilityVerified:true,
  Scope:CaseReviewScope{GuildID:1,ServerID:2,InstallationID:3},
  CaseID:4,ActorUserID:5,ActionKey:strings.Repeat("a",64),
  ExpectedStatus:"PENDING_REVIEW",ToStatus:"REVIEWED",
  ReasonCode:"EVIDENCE_REVIEWED",Note:"Fixture review only",
  At:time.Date(2026,9,27,17,0,0,0,time.UTC),
 }
}

func TestSyntheticReviewRejectsMissingGateBeforeDatabase(t *testing.T){
 var writer *CaseReviewMutation
 x:=validSyntheticReview()
 if ok,err:=writer.ApplySynthetic(nil,x);ok||err==nil{t.Fatal("nil writer accepted")}
 x.FixtureOnly=false
 if ok,err:=writer.ApplySynthetic(nil,x);ok||err!=ErrCASEReviewFixtureDisabled{t.Fatal("fixture flag bypass")}
 x=validSyntheticReview();x.CallerCapabilityVerified=false
 if ok,err:=writer.ApplySynthetic(nil,x);ok||err!=ErrCASEReviewFixtureDisabled{t.Fatal("capability flag bypass")}
}

func TestReviewInputValidationContract(t *testing.T){
 if !reviewValidKey(strings.Repeat("a",64))||reviewValidKey(strings.Repeat("A",64))||
  reviewValidKey(strings.Repeat("a",63))||reviewValidKey(strings.Repeat("/",64)){
  t.Fatal("action key must be exactly 64 lowercase hex chars")
 }
 for _,transition:=range [][2]string{{"PENDING_REVIEW","REVIEWED"},{"PENDING_REVIEW","DISMISSED"},{"REVIEWED","RESOLVED"}}{
  if !reviewTransition(transition[0],transition[1]){t.Fatal("expected neutral transition rejected")}
 }
 for _,transition:=range [][2]string{{"PENDING_REVIEW","RESOLVED"},{"DISMISSED","REVIEWED"},{"RESOLVED","PENDING_REVIEW"},{"REVIEWED","DISMISSED"}}{
  if reviewTransition(transition[0],transition[1]){t.Fatal("invalid transition accepted")}
 }
 for _,tc:=range []struct{reason,to string}{
  {"EVIDENCE_REVIEWED","REVIEWED"},
  {"INSUFFICIENT_EVIDENCE","DISMISSED"},
  {"FALSE_POSITIVE","DISMISSED"},
  {"STAFF_CLOSED","RESOLVED"},
 }{
  if !reviewReasonValid(tc.reason,tc.to){t.Fatal("approved reason rejected")}
 }
 if reviewReasonValid("EVIDENCE_REVIEWED","DISMISSED")||
  reviewReasonValid("AUTOMATIC_CHEATER","REVIEWED"){t.Fatal("unapproved reason accepted")}
}
