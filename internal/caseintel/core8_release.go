package caseintel

// Releasing a detector to staff alerts is two deliberate edits, made only
// after its shadow review passes and the owner approves:
//
//  1. set its Mode to "VALIDATED_SHADOW" in ClientCatalog (detectors.go), and
//  2. add its reviewed thresholds to releasedThresholds below.
//
// Until both are done EvaluateLoginsForAlert can never return CanNotify, so
// the staff alert pipeline enqueues nothing. No detector is released today.
var releasedThresholds = map[string]ValidatedThresholds{}

// ReleasedThresholds returns a copy of a module's approved thresholds, or nil.
func ReleasedThresholds(moduleID string) *ValidatedThresholds {
	t, ok := releasedThresholds[moduleID]
	if !ok || !t.Approved {
		return nil
	}
	return &t
}

// ModuleReleased reports whether a module may produce staff alerts: it is
// marked VALIDATED_SHADOW in the catalog and has approved thresholds.
func ModuleReleased(moduleID string) bool {
	for _, d := range ClientCatalog() {
		if d.ID == moduleID {
			return d.Mode == "VALIDATED_SHADOW" && ReleasedThresholds(moduleID) != nil
		}
	}
	return false
}
