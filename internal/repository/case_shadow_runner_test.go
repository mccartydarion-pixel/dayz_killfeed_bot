package repository

import (
 "context"
 "errors"
 "testing"
)

func TestShadowRunGateFailsBeforeAnyDatabaseAccess(t *testing.T){
 r:=NewShadowLedger(nil)
 _,err:=r.RunBlockedOnLatestSource(context.Background(),ShadowRunInput{
  GuildID:1,ServerID:1,DetectorID:"CASE-MOV-001",Version:"0.1.0",Limit:10,
 })
 if !errors.Is(err,ErrShadowDisabled){t.Fatalf("default-off runner must stop before DB: %v",err)}
}

func TestShadowHistoryRejectsInvalidScopeBeforeDatabase(t *testing.T){
 r:=NewShadowLedger(nil)
 for _,tc:=range []struct{guild,server int64;limit int}{{0,1,10},{1,0,10},{1,1,0},{1,1,51}} {
  if _,err:=r.ListShadowHistory(context.Background(),tc.guild,tc.server,nil,tc.limit);err==nil{
   t.Fatalf("invalid scope unexpectedly accepted: %+v",tc)
  }
 }
}
