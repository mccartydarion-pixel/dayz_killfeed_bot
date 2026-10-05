package caseintel

import (
	"reflect"
	"testing"
)

func TestLoginObservationsUseOnlyRecordedSameSourceConnects(t *testing.T) {
	r := Reconstruct(1, []Event{
		ev(4, 400, "boot-a", "PLAYER_DISCONNECT", pid(1), nil, nil),
		ev(3, 300, "boot-a", "PLAYER_CONNECT", pid(1), nil, nil),
		ev(2, 200, "boot-a", "PLAYER_CONNECT", pid(1), nil, nil),
		ev(1, 100, "boot-a", "PLAYER_CONNECT", pid(1), nil, nil),
		ev(5, 100, "boot-b", "PLAYER_CONNECT", pid(1), nil, nil),
		ev(6, 200, "boot-b", "PLAYER_DISCONNECT", pid(1), nil, nil),
		ev(7, 500, "boot-a", "PLAYER_CONNECT", pid(2), nil, nil),
	}, 200, true)
	got := r.LoginObservations
	if got.Status != "OBSERVATION_ONLY" || got.DetectorsEnabled || !got.WindowTruncated ||
		got.RepeatedConnectWindows != 2 || got.RapidReconnectStatus != "UNVERIFIABLE_ELAPSED_TIME" ||
		got.RestartContext != "UNVERIFIED" || !reflect.DeepEqual(got.EvidenceIDs, []int64{1, 2, 3}) {
		t.Fatalf("invented login inference or wrong source evidence: %+v", got)
	}
}

func TestLoginObservationsDoNotFlagNormalReconnectOrCrossSourceBoundary(t *testing.T) {
	r := Reconstruct(1, []Event{
		ev(1, 100, "boot-a", "PLAYER_CONNECT", pid(1), nil, nil),
		ev(2, 200, "boot-a", "PLAYER_DISCONNECT", pid(1), nil, nil),
		ev(3, 300, "boot-a", "PLAYER_CONNECT", pid(1), nil, nil),
		ev(4, 100, "boot-b", "PLAYER_CONNECT", pid(1), nil, nil),
	}, 200, false)
	got := r.LoginObservations
	if got.RepeatedConnectWindows != 0 || len(got.EvidenceIDs) != 0 || got.DetectorsEnabled || got.RestartContext != "UNVERIFIED" {
		t.Fatalf("normal reconnect/source change flagged: %+v", got)
	}
}
