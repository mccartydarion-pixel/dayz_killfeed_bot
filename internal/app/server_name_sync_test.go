package app

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeNameStore mimics ServerNameRepository: a custom name is never overwritten, the last Nitrado
// name is always recorded.
type fakeNameStore struct {
	targets  []repository.ServerNameTarget
	display  map[int64]string
	custom   map[int64]bool
	provider map[int64]string
	writes   int
	listErr  error
}

func (s *fakeNameStore) ListSyncTargets(context.Context) ([]repository.ServerNameTarget, error) {
	return s.targets, s.listErr
}

func (s *fakeNameStore) RecordProviderName(_ context.Context, id int64, name string) (bool, error) {
	if name == "" {
		return false, errors.New("provider name is empty")
	}
	s.writes++
	s.provider[id] = name
	if s.custom[id] || s.display[id] == name {
		return false, nil
	}
	s.display[id] = name
	return true, nil
}

type fakeNameReader struct {
	names map[string]nitrado.GameserverName
	errs  map[string]error
	calls []string
}

func (r *fakeNameReader) GameserverName(_ context.Context, serviceID string) (nitrado.GameserverName, error) {
	r.calls = append(r.calls, serviceID)
	if err := r.errs[serviceID]; err != nil {
		return nitrado.GameserverName{}, err
	}
	return r.names[serviceID], nil
}

func nameSyncWorld() (*fakeNameStore, *fakeNameReader, serverNameClientFunc, *[]int64) {
	store := &fakeNameStore{display: map[int64]string{}, custom: map[int64]bool{}, provider: map[int64]string{}}
	reader := &fakeNameReader{names: map[string]nitrado.GameserverName{}, errs: map[string]error{}}
	renamed := &[]int64{}
	clientFor := func(_ context.Context, t repository.ServerNameTarget) (serverNameReader, string, error) {
		if t.OrganizationID == nil {
			return nil, "", errors.New("no Nitrado credential")
		}
		return reader, "org:" + string(rune('0'+*t.OrganizationID)), nil
	}
	return store, reader, clientFor, renamed
}

func nameTarget(id, org int64, service string) repository.ServerNameTarget {
	return repository.ServerNameTarget{ServerID: id, GuildID: 1, OrganizationID: &org, ProviderServiceID: service}
}

func TestServerNameSyncFollowsNitradoAndCustomWins(t *testing.T) {
	store, reader, clientFor, renamed := nameSyncWorld()
	store.targets = []repository.ServerNameTarget{nameTarget(1, 1, "101"), nameTarget(2, 1, "102"), nameTarget(3, 1, "103")}
	store.display = map[int64]string{1: "Old name", 2: "Owner's name", 3: "Same"}
	store.custom[2] = true
	reader.names["101"] = nitrado.GameserverName{ServiceID: 101, Hostname: "  New\tname ", QueryName: "Old name"}
	reader.names["102"] = nitrado.GameserverName{ServiceID: 102, Hostname: "Nitrado name"}
	reader.names["103"] = nitrado.GameserverName{ServiceID: 103, QueryName: "Same"}

	res, err := syncServerNames(context.Background(), store, clientFor, func(id int64) { *renamed = append(*renamed, id) }, 0)
	if err != nil {
		t.Fatal(err)
	}
	if store.display[1] != "New name" {
		t.Fatalf("server 1 must follow the Nitrado hostname (cleaned), got %q", store.display[1])
	}
	if store.display[2] != "Owner's name" || store.provider[2] != "Nitrado name" {
		t.Fatalf("a custom name wins, and the Nitrado name is still recorded: display %q provider %q", store.display[2], store.provider[2])
	}
	if store.display[3] != "Same" || store.provider[3] != "Same" {
		t.Fatalf("unchanged server: %q / %q", store.display[3], store.provider[3])
	}
	if res.Servers != 3 || res.Renamed != 1 || res.Unchanged != 2 || len(*renamed) != 1 || (*renamed)[0] != 1 {
		t.Fatalf("unexpected result %+v renamed %v", res, *renamed)
	}
	if len(reader.calls) != 3 {
		t.Fatalf("exactly one Nitrado read per server, got %v", reader.calls)
	}
}

func TestServerNameSyncNeverBlanksOnEmptyOrForeignResponse(t *testing.T) {
	store, reader, clientFor, _ := nameSyncWorld()
	store.targets = []repository.ServerNameTarget{nameTarget(1, 1, "101"), nameTarget(2, 1, "102"), nameTarget(3, 1, "103")}
	store.display = map[int64]string{1: "Keep 1", 2: "Keep 2", 3: "Keep 3"}
	reader.names["101"] = nitrado.GameserverName{ServiceID: 101}                        // no name at all
	reader.names["102"] = nitrado.GameserverName{ServiceID: 102, Hostname: " \r\n "}    // blank after cleaning
	reader.names["103"] = nitrado.GameserverName{ServiceID: 999, Hostname: "Not yours"} // another service
	res, err := syncServerNames(context.Background(), store, clientFor, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if store.writes != 0 || store.display[1] != "Keep 1" || store.display[2] != "Keep 2" || store.display[3] != "Keep 3" {
		t.Fatalf("nothing may be written: writes %d display %v", store.writes, store.display)
	}
	if res.NoName != 2 || res.Failed != 1 || res.Renamed != 0 {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestServerNameSyncFailureKeepsNameAndBacksOffThatCredential(t *testing.T) {
	store, reader, clientFor, _ := nameSyncWorld()
	other := int64(2)
	store.targets = []repository.ServerNameTarget{nameTarget(1, 1, "101"), nameTarget(2, 1, "102"), nameTarget(3, other, "103"),
		{ServerID: 4, GuildID: 9, ProviderServiceID: "104"}} // no credential
	store.display = map[int64]string{1: "Keep 1", 2: "Keep 2", 3: "Old 3", 4: "Keep 4"}
	reader.errs["101"] = &nitrado.RequestError{Op: "gameserver name", Kind: nitrado.KindAuthentication, StatusCode: 401}
	reader.names["102"] = nitrado.GameserverName{ServiceID: 102, Hostname: "Would change"}
	reader.names["103"] = nitrado.GameserverName{ServiceID: 103, Hostname: "New 3"}
	res, err := syncServerNames(context.Background(), store, clientFor, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if store.display[1] != "Keep 1" || store.display[2] != "Keep 2" || store.display[4] != "Keep 4" {
		t.Fatalf("a failed read must change nothing: %v", store.display)
	}
	if store.display[3] != "New 3" {
		t.Fatalf("another credential's server must still sync, got %q", store.display[3])
	}
	if len(reader.calls) != 2 || reader.calls[0] != "101" || reader.calls[1] != "103" {
		t.Fatalf("after a failure the same credential's other servers wait for the next pass, calls %v", reader.calls)
	}
	if res.Failed != 1 || res.Skipped != 1 || res.NoCredential != 1 || res.Renamed != 1 {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestServerNameSyncListFailure(t *testing.T) {
	store, reader, clientFor, _ := nameSyncWorld()
	store.listErr = errors.New("database down")
	if _, err := syncServerNames(context.Background(), store, clientFor, nil, 0); err == nil || len(reader.calls) != 0 {
		t.Fatalf("expected the list error and no Nitrado call, got %v calls %v", err, reader.calls)
	}
}

func TestServerNameSyncWaitIsAboutTwentyMinutes(t *testing.T) {
	for i := 0; i < 200; i++ {
		w := serverNameSyncWait(serverNameSyncInterval)
		if w < serverNameSyncInterval || w >= serverNameSyncInterval+serverNameSyncJitter {
			t.Fatalf("wait %s outside [%s, %s)", w, serverNameSyncInterval, serverNameSyncInterval+serverNameSyncJitter)
		}
	}
}

func TestDayZServerSummaryCarriesNameSource(t *testing.T) {
	s := toDayZServerSummary(repository.GameServer{ID: 1, ProviderServiceID: "42", DisplayName: "Mine", DisplayNameCustom: true, ProviderName: "Theirs"})
	if !s.DisplayNameCustom || s.ProviderName == nil || *s.ProviderName != "Theirs" {
		t.Fatalf("unexpected %+v", s)
	}
	if s := toDayZServerSummary(repository.GameServer{ID: 1, DisplayName: "Followed"}); s.DisplayNameCustom || s.ProviderName != nil {
		t.Fatalf("a never-read Nitrado name is null: %+v", s)
	}
}
