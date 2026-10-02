package nitrado

import "testing"

func TestTailMatchesToleratesGrowthBetweenReads(t *testing.T) {
	full := []byte("abcdef")
	if ok, n := TailMatches(full, 2, []byte("cdef")); !ok || n != 3 {
		t.Fatalf("exact: %v %d", ok, n)
	}
	if ok, n := TailMatches(full, 2, []byte("cdefGH")); !ok || n != 3 {
		t.Fatalf("file grew after the full download: %v %d", ok, n)
	}
	if ok, _ := TailMatches(full, 2, []byte("cdXf")); ok {
		t.Fatal("different bytes must not match")
	}
	if ok, _ := TailMatches(full, 9, []byte("x")); ok {
		t.Fatal("offset past the file")
	}
}

func TestTailTrustLifecycle(t *testing.T) {
	svc := "trust-lifecycle"
	if tailOnly, verify := TailPlan(svc); tailOnly || !verify {
		t.Fatal("a new service verifies")
	}
	TailVerified(svc, true, 0, "SEEK", "test") // no new bytes: does not count
	for i := 0; i < TailTrustAfter; i++ {
		TailVerified(svc, true, 10, "SEEK", "test")
	}
	if !TailTrust()[svc].Trusted {
		t.Fatal("trusted after enough matches")
	}
	tailOnlyReads, verifyReads := 0, 0
	for i := 0; i < TailRecheckEvery; i++ {
		tailOnly, verify := TailPlan(svc)
		if tailOnly {
			tailOnlyReads++
		}
		if verify {
			verifyReads++
		}
	}
	if verifyReads != 1 || tailOnlyReads != TailRecheckEvery-1 {
		t.Fatalf("periodic recheck: tail=%d verify=%d", tailOnlyReads, verifyReads)
	}
	TailFailed(svc)
	if TailTrust()[svc].Trusted {
		t.Fatal("a failed tail read goes back to verifying")
	}
	TailVerified(svc, false, 5, "SEEK", "test")
	if st := TailTrust()[svc]; !st.Disabled {
		t.Fatal("a mismatch disables")
	}
	if tailOnly, verify := TailPlan(svc); tailOnly || verify {
		t.Fatal("disabled services only do full downloads")
	}
}
