package killfeed

import (
	"testing"
	"time"
)

func TestRuntimeDiagnosticsSnapshotAndReset(t *testing.T) {
	diagnostics := NewRuntimeDiagnostics(7)
	diagnostics.Update(func(snapshot *RuntimeDiagnosticSnapshot) {
		snapshot.WorkerRunning = true
		snapshot.SelectedADM = "current.ADM"
		snapshot.LastMetadataChanged = true
		snapshot.LastDownloadAttempt = time.Now()
		snapshot.CompleteLines = 2
		snapshot.LastParsedEventType = "PLAYER_CONNECT"
	})
	diagnostics.Event("metadata checked changed=true")
	snapshot := diagnostics.Snapshot()
	if snapshot.ServerID != 7 || !snapshot.WorkerRunning || snapshot.SelectedADM != "current.ADM" || len(snapshot.RecentEvents) != 1 {
		t.Fatalf("unexpected diagnostics snapshot: %+v", snapshot)
	}
	diagnostics.Reset()
	if len(diagnostics.Snapshot().RecentEvents) != 0 {
		t.Fatal("reset should clear timeline only")
	}
}

func TestRuntimeDiagnosticsClassification(t *testing.T) {
	diagnostics := RuntimeDiagnosticSnapshot{LastMetadataChanged: true}
	if got := diagnostics.Classification(); got != "ADM_DOWNLOAD_FAILURE" {
		t.Fatalf("got %s", got)
	}
}
