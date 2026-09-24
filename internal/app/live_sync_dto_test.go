package app

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Requirement 5: a location line ingested late is not presented as fresh. With a known source UTC
// time, age and freshness are measured from when DayZ says it happened, not from ingestion.
func TestLocationDTOAgesFromSourceTimeWhenKnown(t *testing.T) {
	ingested := time.Now().Add(-5 * time.Second)
	happened := time.Now().Add(-3 * time.Hour)
	dto := toLocationDTO(&repository.LocationEvent{X: 1, Z: 2, EventType: "PLAYER_LIST", ObservedAt: ingested, SourceUTC: &happened})
	if dto.TimeBasis != "SOURCE" || dto.OccurredAt == nil || dto.AgeSeconds < 3*3600-5 || dto.Freshness != repository.LocationFreshnessStale {
		t.Fatalf("late line must be aged from its source time: %+v", dto)
	}
	// Without a known offset the ingestion time is the only basis, and it is labeled as such.
	dto = toLocationDTO(&repository.LocationEvent{X: 1, Z: 2, EventType: "PLAYER_LIST", ObservedAt: ingested})
	if dto.TimeBasis != "INGESTION" || dto.OccurredAt != nil {
		t.Fatalf("ingestion basis: %+v", dto)
	}
	// A source time after ingestion (clock error) is never trusted.
	future := time.Now().Add(time.Hour)
	dto = toLocationDTO(&repository.LocationEvent{X: 1, Z: 2, EventType: "PLAYER_LIST", ObservedAt: ingested, SourceUTC: &future})
	if dto.TimeBasis != "INGESTION" {
		t.Fatalf("future source time: %+v", dto)
	}
}
