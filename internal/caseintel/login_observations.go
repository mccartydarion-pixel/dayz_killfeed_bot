package caseintel

// LoginObservationReport summarizes bounded source-order observations for the
// Suspicious Logins module. It is not a detector output or a player verdict.
// ADM clocks cannot establish a rapid reconnect or a server restart.
type LoginObservationReport struct {
	Status                 string  `json:"status"`
	RepeatedConnectWindows int     `json:"repeatedConnectWindows"`
	EvidenceIDs            []int64 `json:"evidenceIds"`
	RapidReconnectStatus   string  `json:"rapidReconnectStatus"`
	RestartContext         string  `json:"restartContext"`
	WindowTruncated        bool    `json:"windowTruncated"`
	DetectorsEnabled       bool    `json:"detectorsEnabled"`
}

// AssessLoginObservations reuses the exact-player, per-source reconstruction.
// One repeated-connect flag contributes its two recorded connect lines; no
// cross-source stitching or ingestion-time duration inference is performed.
func AssessLoginObservations(r Reconstruction) LoginObservationReport {
	out := LoginObservationReport{Status: "OBSERVATION_ONLY", EvidenceIDs: []int64{},
		RapidReconnectStatus: "UNVERIFIABLE_ELAPSED_TIME", RestartContext: "UNVERIFIED",
		WindowTruncated: r.WindowTruncated, DetectorsEnabled: false}
	seen := map[int64]bool{}
	for _, src := range r.Sources {
		for i, w := range src.ConnectionWindows {
			if !hasFlag(w.Flags, "REPEATED_CONNECT_WITHOUT_DISCONNECT") {
				continue
			}
			out.RepeatedConnectWindows++
			for _, obs := range w.Observations {
				if !isConnect(obs) || obs.EvidenceID <= 0 || seen[obs.EvidenceID] {
					continue
				}
				seen[obs.EvidenceID] = true
				out.EvidenceIDs = append(out.EvidenceIDs, obs.EvidenceID)
			}
			// The next window's first connect closed this one in source order.
			if i+1 < len(src.ConnectionWindows) {
				next := src.ConnectionWindows[i+1]
				if next.StartReason == "CONNECT" && len(next.Observations) > 0 {
					obs := next.Observations[0]
					if isConnect(obs) && obs.EvidenceID > 0 && !seen[obs.EvidenceID] {
						seen[obs.EvidenceID] = true
						out.EvidenceIDs = append(out.EvidenceIDs, obs.EvidenceID)
					}
				}
			}
		}
	}
	return out
}
