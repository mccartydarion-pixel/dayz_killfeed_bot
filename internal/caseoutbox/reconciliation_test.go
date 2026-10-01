package caseoutbox

import (
 "strings"
 "testing"
 "time"
)

func TestReconcileExpiredSyntheticRequiresVerifiedExactReceipt(t *testing.T) {
 now:=time.Date(2026,9,28,1,0,0,0,time.UTC)
 item,err:=NewSynthetic(strings.Repeat("a",64),Scope{GuildID:1,InstallationID:2,ServerID:3},now)
 if err!=nil {t.Fatal(err)}
 leased,lease,err:=Acquire(item,now)
 if err!=nil {t.Fatal(err)}
 p:=ReconciliationProof{Key:item.Key,Scope:item.Scope,LeaseVersion:lease.Version,
  ObservedAt:lease.Until.Add(time.Second),Outcome:DeliveryConfirmed,Verified:true}
 done,err:=ReconcileExpiredSynthetic(leased,lease,p)
 if err!=nil||done.Status!=Sent||done.SentAt.IsZero()||done.Attempts!=1||
  done.Key!=item.Key||done.Scope!=item.Scope {t.Fatalf("confirmed delivery: %+v %v",done,err)}
 if _,_,err:=Acquire(done,p.ObservedAt.Add(time.Hour));err==nil{t.Fatal("confirmed delivery reclaimed")}
 if _,err:=ReconcileExpiredSynthetic(done,lease,p);err==nil{t.Fatal("replayed receipt accepted")}
 checks:=[]struct{name string;mutate func(*ReconciliationProof)}{
  {"unverified",func(x *ReconciliationProof){x.Verified=false}},
  {"wrong delivery key",func(x *ReconciliationProof){x.Key=strings.Repeat("b",64)}},
  {"wrong installation",func(x *ReconciliationProof){x.Scope.InstallationID=99}},
  {"wrong lease version",func(x *ReconciliationProof){x.LeaseVersion++}},
  {"early receipt",func(x *ReconciliationProof){x.ObservedAt=lease.Until.Add(-time.Nanosecond)}},
  {"missing timestamp",func(x *ReconciliationProof){x.ObservedAt=time.Time{}}},
  {"ambiguous",func(x *ReconciliationProof){x.Outcome=DeliveryUncertain}},
  {"unknown outcome",func(x *ReconciliationProof){x.Outcome="SENT_MAYBE"}},
 }
 for _,c:=range checks {t.Run(c.name,func(t *testing.T) {
  q:=p;c.mutate(&q)
  unchanged,e:=ReconcileExpiredSynthetic(leased,lease,q)
  if e==nil||unchanged.Status!=Leased||unchanged.LeaseVersion!=leased.LeaseVersion{
   t.Fatalf("unsafe reconciliation: %+v %v",unchanged,e)
  }
 })}
}

func TestConfirmedNonDeliveryIsBoundedAndDoesNotPretendReceipt(t *testing.T){
 item,now:=start(t)
 leased,lease,err:=Acquire(item,now)
 if err!=nil{t.Fatal(err)}
 p:=ReconciliationProof{Key:item.Key,Scope:item.Scope,LeaseVersion:lease.Version,
  ObservedAt:lease.Until.Add(time.Second),Outcome:NonDeliveryConfirmed,Verified:true}
 next,err:=ReconcileExpiredSynthetic(leased,lease,p)
 if err!=nil||next.Status!=RetryWait||!next.SentAt.IsZero()||
  !next.NextAt.Equal(p.ObservedAt.Add(30*time.Second))||
  next.LastErrorCode!="VERIFIED_NON_DELIVERY"{t.Fatalf("retry: %+v %v",next,err)}
 if _,_,err:=Acquire(next,next.NextAt.Add(-time.Nanosecond));err==nil{t.Fatal("early retry")}
 for attempt:=2;attempt<=MaxAttempts;attempt++ {
  claimed,l,e:=Acquire(next,next.NextAt)
  if e!=nil{t.Fatal(e)}
  proof:=ReconciliationProof{Key:claimed.Key,Scope:claimed.Scope,LeaseVersion:l.Version,
   ObservedAt:l.Until.Add(time.Second),Outcome:NonDeliveryConfirmed,Verified:true}
  next,e=ReconcileExpiredSynthetic(claimed,l,proof)
  if e!=nil{t.Fatal(e)}
 }
 if next.Status!=Dead||next.Attempts!=MaxAttempts||!next.NextAt.IsZero(){
  t.Fatalf("non-delivery attempt budget not terminal: %+v",next)
 }
 if _,_,err:=Acquire(next,now.Add(time.Hour));err==nil{t.Fatal("dead item reclaimed")}
}
