package app

import (
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"
)

func TestCASEReviewWriteIsUnavailableUntilDetectorRelease(t *testing.T){
 for _,flag:=range []string{"","false","true","TRUE"}{
  t.Run("flag_"+flag,func(t *testing.T){
   t.Setenv("CHAMPION_CASE_REVIEW_WRITES_ENABLED",flag)
   if caseReviewWritesAvailable(){t.Fatal("unvalidated current detector registry exposed review write")}
   req:=httptest.NewRequest(http.MethodPost,"/api/saas/anti-cheat/cases/1/review",
    strings.NewReader(`{"toStatus":"REVIEWED"}`))
   rec:=httptest.NewRecorder()
   (&App{}).handleAntiCheatCaseReview(rec,req)
   if rec.Code!=http.StatusNotFound{t.Fatalf("disabled route returned %d",rec.Code)}
  })
 }
}
