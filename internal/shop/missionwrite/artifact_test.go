package missionwrite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

var workerBinding = capability.Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: testService}

const secondBoot = "DayZServer_PS4_x64_2026-09-26_05-25-31.ADM"

func stagedPayload() []byte {
	return nitradodelivery.SpawnerFile{Objects: nitradodelivery.AttemptEntries("champion:d42:a1", "BandageDressing", 2, [3]float64{4621.1, 319.6, 8397.2})}.Render()
}

func inspectOK(t *testing.T, s *standIn) ArtifactState {
	t.Helper()
	st, err := InspectArtifact(context.Background(), s.client(), workerBinding)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return st
}

func TestInspectArtifactReadsOneConsistentSnapshot(t *testing.T) {
	s := afterGateD(t)
	st := inspectOK(t, s)
	if st.SHA256 != emptySHA || string(st.Content) != champEmpty || st.ConfigSHA256 != SHA256(relocated(t)) || st.GameserverStatus != "started" ||
		st.MissionPath == "" || len(st.Boots) != 1 || st.CurrentBoot() != "DayZServer_PS4_x64_2026-09-26_04-17-25.ADM" {
		t.Fatalf("state = %+v", st)
	}
	if len(s.uploadCalls)+len(s.transfers)+len(s.mkdirCalls) != 0 {
		t.Fatal("an inspection wrote something")
	}
	// A second boot is listed after the first: the newest is current.
	s.setFile(configDir+"/"+secondBoot, []byte("AdminLog started\n"))
	if st := inspectOK(t, s); len(st.Boots) != 2 || st.CurrentBoot() != secondBoot {
		t.Fatalf("boots = %v", st.Boots)
	}
	// The printed state never carries an account path.
	if printed := strings.ToLower(strings.Join(st.Boots, " ") + st.MissionPath); strings.Contains(printed, "ni1_1") {
		t.Fatalf("state exposes the account path: %s", printed)
	}
}

func TestInspectArtifactFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(t *testing.T) *standIn
		want    error
	}{
		"the file was removed":            {func(t *testing.T) *standIn { s := afterGateD(t); delete(s.files, customChamp); return s }, ErrArtifactMissing},
		"not referenced (before Gate D)":  {func(t *testing.T) *standIn { return afterGateC(t) }, ErrArtifactNotReferenced},
		"configuration is not valid JSON": {func(t *testing.T) *standIn { s := afterGateD(t); s.files[cfgFile] = []byte("{"); return s }, ErrInspection},
		"listings fail":                   {func(t *testing.T) *standIn { s := afterGateD(t); s.failLists = true; return s }, nil},
		"a directory where the file would be": {func(t *testing.T) *standIn {
			s := afterGateD(t)
			delete(s.files, customChamp)
			s.dirs[customChamp] = true
			return s
		}, nil},
	} {
		s := tc.prepare(t)
		_, err := InspectArtifact(context.Background(), s.client(), workerBinding)
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
		if len(s.uploadCalls)+len(s.transfers)+len(s.mkdirCalls) != 0 {
			t.Errorf("%s: a failed inspection wrote something", name)
		}
	}
}

func TestWriteArtifactStagesAndUnstages(t *testing.T) {
	s := afterGateD(t)
	ctx := context.Background()
	payload := stagedPayload()

	w, err := WriteArtifact(ctx, s.client(), workerBinding, inspectOK(t, s), payload)
	if err != nil || w.Status != StatusWrittenVerified || w.Before != emptySHA || w.After != SHA256(payload) || w.ConfigChanged || w.RestartDuring || w.Unverified {
		t.Fatalf("stage: %v %+v", err, w)
	}
	if got, _ := s.file(customChamp); string(got) != string(payload) {
		t.Fatalf("stored %s", got)
	}
	if got, _ := s.file(cfgFile); string(got) != string(relocated(t)) {
		t.Fatal("a worker write touched the configuration")
	}
	if len(s.uploadCalls) != 1 || len(s.transfers) != 1 || len(s.mkdirCalls) != 0 {
		t.Fatalf("token requests=%d transfers=%d mkdirs=%d, want exactly one write", len(s.uploadCalls), len(s.transfers), len(s.mkdirCalls))
	}

	// Unstage: back to the empty file, from a fresh snapshot.
	w, err = WriteArtifact(ctx, s.client(), workerBinding, inspectOK(t, s), []byte(champEmpty))
	if err != nil || w.Status != StatusWrittenVerified || w.Before != SHA256(payload) || w.After != emptySHA {
		t.Fatalf("unstage: %v %+v", err, w)
	}
}

func TestWriteArtifactRefusesAStaleSnapshot(t *testing.T) {
	for name, change := range map[string]func(s *standIn){
		"the file changed":          func(s *standIn) { s.files[customChamp] = stagedPayload() },
		"the configuration changed": func(s *standIn) { s.files[cfgFile] = []byte(strings.Replace(string(s.files[cfgFile]), "\n", "\n ", 1)) },
		"a new boot appeared":       func(s *standIn) { s.setFile(configDir+"/"+secondBoot, []byte("AdminLog started\n")) },
		"the server stopped":        func(s *standIn) { s.status = "stopped" },
	} {
		s := afterGateD(t)
		st := inspectOK(t, s)
		change(s)
		w, err := WriteArtifact(context.Background(), s.client(), workerBinding, st, stagedPayload())
		if !errors.Is(err, ErrSnapshotStale) || w.Status != StatusNotWritten {
			t.Errorf("%s: %v %+v", name, err, w)
		}
		if len(s.uploadCalls)+len(s.transfers) != 0 {
			t.Errorf("%s: a write was attempted on a stale snapshot", name)
		}
	}
	// A payload that is not a Champion spawner file is never sent.
	s := afterGateD(t)
	for _, bad := range []string{`{"Objects":[{"name":"Land_Castle","customString":"mine"}]}`, `not json`} {
		if _, err := WriteArtifact(context.Background(), s.client(), workerBinding, inspectOK(t, s), []byte(bad)); err == nil || len(s.uploadCalls) != 0 {
			t.Errorf("payload %q: err=%v uploads=%d", bad, err, len(s.uploadCalls))
		}
	}
	// A zero snapshot (no inspection) is refused.
	if _, err := WriteArtifact(context.Background(), s.client(), workerBinding, ArtifactState{}, stagedPayload()); err == nil {
		t.Error("a write without an inspection was accepted")
	}
}

func TestWriteArtifactOutcomeIsDecidedByReadBack(t *testing.T) {
	payload := stagedPayload()
	for name, tc := range map[string]struct {
		knob   func(s *standIn)
		status string
		stored string // "" = unchanged empty file
	}{
		"token refused":                    {func(s *standIn) { s.tokenStatus = 500 }, StatusNotWritten, ""},
		"transfer refused":                 {func(s *standIn) { s.transferStatus = 500 }, StatusNotWritten, ""},
		"connection dropped before store":  {func(s *standIn) { s.dropBefore = true }, StatusNotWritten, ""},
		"connection dropped after store":   {func(s *standIn) { s.dropAfter = true }, StatusWrittenVerified, string(payload)},
		"success reported, nothing stored": {func(s *standIn) { s.claimNoStore = true }, StatusUncertain, ""},
		"half the bytes stored":            {func(s *standIn) { s.partialStore = true }, StatusUncertain, string(payload[:len(payload)/2])},
	} {
		s := afterGateD(t)
		st := inspectOK(t, s)
		tc.knob(s)
		w, err := WriteArtifact(context.Background(), s.client(), workerBinding, st, payload)
		if err != nil || w.Status != tc.status {
			t.Errorf("%s: err=%v status=%s detail=%s, want %s", name, err, w.Status, w.Detail, tc.status)
		}
		want := tc.stored
		if want == "" {
			want = champEmpty
		}
		if got, _ := s.file(customChamp); string(got) != want {
			t.Errorf("%s: stored %q", name, got)
		}
		if len(s.transfers) > 1 || len(s.uploadCalls) > 1 {
			t.Errorf("%s: the write was retried (%d tokens, %d transfers)", name, len(s.uploadCalls), len(s.transfers))
		}
		for _, secret := range []string{"http://", "https://", "api-secret-standin", "ni1_1"} {
			if strings.Contains(w.Detail, secret) {
				t.Errorf("%s: the detail leaks %q: %s", name, secret, w.Detail)
			}
		}
	}

	// The read-back itself fails after the write request: uncertain, never assumed either way.
	s := afterGateD(t)
	st := inspectOK(t, s)
	s.afterTransfer = func(s *standIn) { s.failLists = true }
	if w, err := WriteArtifact(context.Background(), s.client(), workerBinding, st, payload); err != nil || w.Status != StatusUncertain || w.After != "" {
		t.Fatalf("unreadable read-back: %v %+v", err, w)
	}
}

func TestWriteArtifactReportsWhatChangedDuringTheWrite(t *testing.T) {
	payload := stagedPayload()

	s := afterGateD(t)
	st := inspectOK(t, s)
	s.afterTransfer = func(s *standIn) { s.setFile(configDir+"/"+secondBoot, []byte("AdminLog started\n")) }
	if w, err := WriteArtifact(context.Background(), s.client(), workerBinding, st, payload); err != nil || w.Status != StatusWrittenVerified || !w.RestartDuring || w.ConfigChanged {
		t.Fatalf("restart during the write: %v %+v", err, w)
	}

	s = afterGateD(t)
	st = inspectOK(t, s)
	s.afterTransfer = func(s *standIn) { s.setFile(cfgFile, []byte(liveConfig)) }
	if w, err := WriteArtifact(context.Background(), s.client(), workerBinding, st, payload); err != nil || w.Status != StatusWrittenVerified || !w.ConfigChanged || w.RestartDuring {
		t.Fatalf("configuration changed during the write: %v %+v", err, w)
	}
}
