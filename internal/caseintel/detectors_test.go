package caseintel

import "testing"

func TestDetectorRegistryCannotBeMutatedAndFailsClosed(t *testing.T) {
 defs:=Registry()
 if len(defs)!=1||defs[0].Mode!="BLOCKED" {t.Fatalf("unexpected registry: %+v",defs)}
 defs[0].Mode="ENABLED";defs[0].Prerequisites[0]="TRUSTED"
 fresh:=Registry()
 if fresh[0].Mode!="BLOCKED"||fresh[0].Prerequisites[0]!="VERIFIED_EVENT_ELAPSED_TIME"{t.Fatal("registry mutated")}
 out:=EvaluatePrerequisites(fresh[0],QualityReport{WindowTruncated:true})
 if out.Status!="BLOCKED"||out.Enforcement!="DISABLED"||out.RiskScore!=nil||
 len(out.Findings)!=0||len(out.EvidenceIDs)!=0||len(out.Blockers)!=5 {t.Fatalf("unsafe result: %+v",out)}
 for _,p:=range out.Blockers {if p.Status!="UNSATISFIED"||p.Reason==""{t.Fatalf("missing blocker reason: %+v",p)}}
}

func TestEvidenceFingerprintScopedDeterministicAndRejectsDuplicates(t *testing.T) {
 a,err:=EvidenceFingerprint(11,22,"CASE-MOV-001","0.1.0",[]int64{4,2,3})
 if err!=nil{t.Fatal(err)}
 b,err:=EvidenceFingerprint(11,22,"CASE-MOV-001","0.1.0",[]int64{2,3,4})
 if err!=nil||a!=b {t.Fatal("same evidence must have stable identity")}
 other,err:=EvidenceFingerprint(11,23,"CASE-MOV-001","0.1.0",[]int64{2,3,4})
 if err!=nil||a==other {t.Fatal("server boundary must change fingerprint")}
 other,err=EvidenceFingerprint(11,22,"CASE-MOV-001","0.2.0",[]int64{2,3,4})
 if err!=nil||a==other {t.Fatal("version must change fingerprint")}
 for _,ids:=range [][]int64{{},{1,1},{0},{-1}} {
  if _,err:=EvidenceFingerprint(11,22,"CASE-MOV-001","0.1.0",ids);err==nil{t.Fatalf("accepted bad evidence IDs: %v",ids)}
 }
}
