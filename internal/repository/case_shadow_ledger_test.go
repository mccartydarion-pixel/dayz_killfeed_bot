package repository

import (
 "context"
 "errors"
 "testing"
)

func TestShadowLedgerDisabledBeforeDatabaseAccess(t *testing.T){
 r:=NewShadowLedger(nil)
 id,err:=r.RecordBlocked(context.Background(),BlockedShadowInput{
  GuildID:1,ServerID:1,DetectorID:"CASE-MOV-001",Version:"0.1.0",EvidenceIDs:[]int64{1},
 })
 if id!=0||!errors.Is(err,ErrShadowDisabled){t.Fatalf("must fail closed without gate: id=%d err=%v",id,err)}
}
func TestShadowLedgerRejectsUnregisteredDetectorBeforeDatabase(t *testing.T){
 r:=NewShadowLedger(nil)
 _,err:=r.RecordBlocked(context.Background(),BlockedShadowInput{
  Enabled:true,GuildID:1,ServerID:1,DetectorID:"CASE-UNKNOWN",Version:"0.1.0",EvidenceIDs:[]int64{1},
 })
 if err==nil {t.Fatal("unknown detector unexpectedly accepted")}
}
