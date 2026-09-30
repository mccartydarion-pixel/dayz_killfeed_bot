package missionwrite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

var relocatedSpawners = []string{"custom/The_Lost_City.json", "custom/champion_shop_delivery.json"}

// afterGateD is Champions now: the config references custom/champion_shop_delivery.json, which is empty.
func afterGateD(t *testing.T) *standIn {
	s := afterGateC(t)
	s.files[cfgFile] = relocated(t)
	return s
}

func canaryAttempt(state string) *AttemptFacts {
	return &AttemptFacts{AttemptID: "champion:d42:a1", State: state, ClassName: "BandageDressing", Quantity: 1,
		Pos: [3]float64{4621.1, 319.6, 8397.2}, ArtifactPath: nitradodelivery.ArtifactRelPath}
}

func attemptReq(t *testing.T, op Operation, a *AttemptFacts) Request {
	staged, empty := AttemptFiles(*a)
	cur, pay := SHA256(empty), SHA256(staged)
	if op == OpUnstageItem {
		cur, pay = SHA256(staged), SHA256(empty)
	}
	return Request{
		Operation:           op,
		Binding:             capability.Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: testService},
		Mission:             testMission,
		Path:                "custom/champion_shop_delivery.json",
		ExpectCurrent:       cur,
		ExpectConfigSHA256:  SHA256(relocated(t)),
		ExpectSpawners:      relocatedSpawners,
		ExpectPayloadSHA256: pay,
		Attempt:             a,
	}
}

func TestGateEStagesExactlyTheAttempt(t *testing.T) {
	s := afterGateD(t)
	a := canaryAttempt(repository.AttemptFilePrepared)
	r, j := attemptReq(t, OpStageItem, a), journal(t)
	p := plan(t, s, r)
	staged, _ := AttemptFiles(*a)
	if !strings.Contains(string(staged), "champion:d42:a1") || !strings.Contains(string(staged), "BandageDressing") {
		t.Fatalf("staged file: %s", staged)
	}
	o, err := Execute(context.Background(), s.client(), r, p.ID, j)
	if err != nil || o.Status != StatusWrittenVerified || o.Before != emptySHA || o.After != SHA256(staged) {
		t.Fatalf("%v %+v", err, o)
	}
	if got, _ := s.file(customChamp); string(got) != string(staged) {
		t.Fatalf("stored %s", got)
	}
	if got, _ := s.file(cfgFile); string(got) != string(relocated(t)) {
		t.Fatal("staging must not touch the configuration")
	}
	if check(o, "cfggameplay.json unchanged") != "PASS" || check(o, "no restart") != "PASS" || len(s.mkdirCalls) != 0 {
		t.Fatalf("%+v", o.Checks)
	}
	// Duplicate: the ID is dead; a new plan is refused (the file is no longer empty).
	if _, err := Execute(context.Background(), s.client(), r, p.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := Prepare(context.Background(), s.client(), r); !errors.Is(err, ErrUnexpectedState) {
		t.Fatalf("restage: %v", err)
	}
}

func TestGateERefusals(t *testing.T) {
	ctx := context.Background()
	for name, mut := range map[string]func(r *Request){
		"plan only (not prepared)":  func(r *Request) { r.Attempt.State = repository.AttemptPlanCreated },
		"already staged":            func(r *Request) { r.Attempt.State = repository.AttemptFileStaged },
		"legacy artifact":           func(r *Request) { r.Attempt.ArtifactPath = nitradodelivery.LegacyArtifactRelPath },
		"preview attempt (d0)":      func(r *Request) { r.Attempt.AttemptID = "champion:d0:a1" },
		"not a Champion attempt id": func(r *Request) { r.Attempt.AttemptID = "x:d42:a1" },
		"no attempt":                func(r *Request) { r.Attempt = nil },
		"wrong payload hash":        func(r *Request) { r.ExpectPayloadSHA256 = strings.Repeat("a", 64) },
		"wrong expected current":    func(r *Request) { r.ExpectCurrent = strings.Repeat("b", 64) },
		"caller payload differs":    func(r *Request) { r.Payload = []byte(`{"Objects":[]}`) },
		"legacy destination":        func(r *Request) { r.Path = "champion/champion_shop_delivery.json" },
	} {
		s := afterGateD(t)
		r := attemptReq(t, OpStageItem, canaryAttempt(repository.AttemptFilePrepared))
		mut(&r)
		if _, err := Prepare(ctx, s.client(), r); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(s.uploadCalls)+len(s.transfers) != 0 {
			t.Errorf("%s: a write was attempted", name)
		}
	}
	// The file is not the empty file (another attempt staged, or foreign content): refused.
	for name, content := range map[string]string{
		"another attempt staged": func() string {
			st, _ := AttemptFiles(AttemptFacts{AttemptID: "champion:d7:a1", ClassName: "BandageDressing", Quantity: 1, Pos: [3]float64{1, 2, 3}})
			return string(st)
		}(),
		"foreign content": `{"Objects":[{"name":"x"}]}`,
	} {
		s := afterGateD(t)
		s.files[customChamp] = []byte(content)
		if _, err := Prepare(ctx, s.client(), attemptReq(t, OpStageItem, canaryAttempt(repository.AttemptFilePrepared))); !errors.Is(err, ErrUnexpectedState) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The configuration does not reference the custom/ file (Gate D not done): refused.
	s := afterGateC(t)
	r := attemptReq(t, OpStageItem, canaryAttempt(repository.AttemptFilePrepared))
	r.ExpectConfigSHA256, r.ExpectSpawners = SHA256(gateBConfig(t)), legacySpawners
	if _, err := Prepare(ctx, s.client(), r); !errors.Is(err, ErrNotReferenced) {
		t.Fatalf("unreferenced: %v", err)
	}
	// A different attempt gets a different plan ID.
	s = afterGateD(t)
	p1 := plan(t, s, attemptReq(t, OpStageItem, canaryAttempt(repository.AttemptFilePrepared)))
	b := canaryAttempt(repository.AttemptFilePrepared)
	b.AttemptID = "champion:d43:a1"
	if p2 := plan(t, s, attemptReq(t, OpStageItem, b)); p2.ID == p1.ID {
		t.Fatal("plan IDs must bind the attempt")
	}
}

func TestGateEInterruptions(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		setup  func(s *standIn)
		status string
	}{
		{"partial", func(s *standIn) { s.partialStore = true }, StatusUncertain},
		{"dropped before storing", func(s *standIn) { s.dropBefore = true }, StatusNotWritten},
		{"dropped after storing", func(s *standIn) { s.dropAfter = true }, StatusWrittenVerified},
		{"success claimed, unchanged", func(s *standIn) { s.claimNoStore = true }, StatusUncertain},
		{"token refused", func(s *standIn) { s.tokenStatus = 403 }, StatusNotWritten},
	} {
		s := afterGateD(t)
		r, j := attemptReq(t, OpStageItem, canaryAttempt(repository.AttemptFilePrepared)), journal(t)
		p := plan(t, s, r)
		c.setup(s)
		o, err := Execute(ctx, s.client(), r, p.ID, j)
		if err != nil || o.Status != c.status || len(s.transfers) > 1 {
			t.Errorf("%s: %v %+v", c.name, err, o)
			continue
		}
		if c.status == StatusNotWritten {
			if got, _ := s.file(customChamp); string(got) != champEmpty {
				t.Errorf("%s: file changed", c.name)
			}
		}
	}
}

func TestGateGUnstagesBackToTheEmptyFile(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{repository.AttemptUnstageRequired, repository.AttemptAwaitingRestart, repository.AttemptFileStaged} {
		s := afterGateD(t)
		a := canaryAttempt(state)
		staged, empty := AttemptFiles(*a)
		s.files[customChamp] = staged
		r, j := attemptReq(t, OpUnstageItem, a), journal(t)
		p := plan(t, s, r)
		o, err := Execute(ctx, s.client(), r, p.ID, j)
		if err != nil || o.Status != StatusWrittenVerified || o.After != emptySHA {
			t.Fatalf("%s: %v %+v", state, err, o)
		}
		if got, _ := s.file(customChamp); string(got) != string(empty) {
			t.Fatalf("%s: not empty", state)
		}
	}
	// Refusals: wrong state, or the file is not exactly THIS attempt's staged file.
	for _, state := range []string{repository.AttemptFilePrepared, repository.AttemptRestartObserved, repository.AttemptVerificationRequired, repository.AttemptFulfilled} {
		s := afterGateD(t)
		a := canaryAttempt(state)
		st, _ := AttemptFiles(*a)
		s.files[customChamp] = st
		if _, err := Prepare(ctx, s.client(), attemptReq(t, OpUnstageItem, a)); !errors.Is(err, ErrAttemptState) {
			t.Errorf("unstage from %s: %v", state, err)
		}
	}
	s := afterGateD(t)
	other := canaryAttempt(repository.AttemptUnstageRequired)
	other.AttemptID = "champion:d43:a1"
	st, _ := AttemptFiles(*other)
	s.files[customChamp] = st
	if _, err := Prepare(ctx, s.client(), attemptReq(t, OpUnstageItem, canaryAttempt(repository.AttemptUnstageRequired))); !errors.Is(err, ErrUnexpectedState) {
		t.Fatalf("another attempt's file: %v", err)
	}
	// Already empty: nothing to unstage.
	s = afterGateD(t)
	if _, err := Prepare(ctx, s.client(), attemptReq(t, OpUnstageItem, canaryAttempt(repository.AttemptUnstageRequired))); !errors.Is(err, ErrUnexpectedState) {
		t.Fatalf("already empty: %v", err)
	}
}

// The full file cycle with one journal: stage, then unstage, distinct single-use plan IDs.
func TestStageUnstageCycle(t *testing.T) {
	ctx := context.Background()
	s := afterGateD(t)
	j := journal(t)
	stage := attemptReq(t, OpStageItem, canaryAttempt(repository.AttemptFilePrepared))
	ps := plan(t, s, stage)
	if o, err := Execute(ctx, s.client(), stage, ps.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("stage: %v %+v", err, o)
	}
	unstage := attemptReq(t, OpUnstageItem, canaryAttempt(repository.AttemptUnstageRequired))
	if _, err := Execute(ctx, s.client(), unstage, ps.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("the stage ID must never authorize the unstage: %v", err)
	}
	pu := plan(t, s, unstage)
	if o, err := Execute(ctx, s.client(), unstage, pu.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("unstage: %v %+v", err, o)
	}
	if got, _ := s.file(customChamp); string(got) != champEmpty {
		t.Fatal("the cycle must end with the empty file")
	}
}
