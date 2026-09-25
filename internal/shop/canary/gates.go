package canary

// Owner gates. Each is a separate, explicit approval; approving one never implies another. Nothing
// in this package performs a gate - it only describes it.
const (
	GateA = "A" // configuration modification
	GateB = "B" // Champion spawner file upload (staging)
	GateC = "C" // first restart
	GateD = "D" // unstaging / cleanup
	GateE = "E" // second restart
	GateF = "F" // final fulfillment
)

// WriteCapability stays UNVERIFIED until an authorized live upload has been read back with the
// expected SHA-256. A listed FILEBROWSER_WRITE role is documentation, not proof.
const WriteCapability = "UNVERIFIED"

// Gate is one approval request.
type Gate struct {
	ID            string
	Title         string
	Action        string
	Writes        bool // performs a production write
	Restarts      bool // needs a server start
	Preconditions []string
	Evidence      []string // what must be shown before the next gate may be requested
	Rollback      string
}

// Gates returns the six gates in order.
func Gates() []Gate {
	return []Gate{
		{ID: GateA, Title: "Configuration modification", Writes: true,
			Action:        "back up cfggameplay.json, then upload the proposed patch (objectSpawnersArr gains " + "champion/champion_shop_delivery.json" + ")",
			Preconditions: []string{"the live file still has the reviewed current SHA-256", "a verified backup exists", "the owner does not edit the file during the window"},
			Evidence:      []string{"read-back SHA-256 equals the proposed SHA-256", "the next boot's RPT shows no [::SpawnObjects] error other than a missing Champion file (if Gate B has not run)"},
			Rollback:      "upload the verified backup and confirm the current SHA-256; effective at the next start"},
		{ID: GateB, Title: "Champion file upload (stage BandageDressing x1)", Writes: true,
			Action: "upload champion/champion_shop_delivery.json with exactly the reviewed staged content",
			Preconditions: []string{"Gate A verified", "a drop point VERIFIED_CURRENT_SESSION taken immediately before", "the canary delivery record exists (its own approval)",
				"the durable attempt ledger is deployed (its own approval) or the operator records every transition", "no restart.log pre-start pending and the current boot started more than the staging quiet period ago"},
			Evidence: []string{"read-back SHA-256 equals the staged SHA-256", "attempt FILE_STAGED -> AWAITING_RESTART recorded"},
			Rollback: "before any start: upload the unstaged (empty) content, verify its SHA-256, attempt -> UNSTAGED"},
		{ID: GateC, Title: "First restart", Restarts: true,
			Action:        "the owner restarts the server (or approves that the next scheduled restart is the canary restart)",
			Preconditions: []string{"Gate B verified", "an operator is available to unstage within the same session"},
			Evidence:      []string{"boot authority accepts a new boot that started after the stage", "that boot's RPT reached CE init with no [::SpawnObjects] error for the Champion file"},
			Rollback:      "none: a started server may have spawned the item; continue to Gate D"},
		{ID: GateD, Title: "Unstaging / cleanup", Writes: true,
			Action:        "upload the unstaged Champion file (attempt removed) and verify its SHA-256",
			Preconditions: []string{"attempt UNSTAGE_REQUIRED", "the file still has the staged SHA-256 (else stop and review)"},
			Evidence:      []string{"read-back SHA-256 equals the unstaged SHA-256 before any further start", "attempt -> VERIFICATION_REQUIRED"},
			Rollback:      "none needed: the unstaged file is the safe state"},
		{ID: GateE, Title: "Second restart", Restarts: true,
			Action:        "one more start with the entry removed, proving it does not spawn again",
			Preconditions: []string{"Gate D verified"},
			Evidence:      []string{"a new boot after the verified unstage, CE init reached, no Champion spawner error", "a single BandageDressing at the drop point, not two"},
			Rollback:      "not applicable"},
		{ID: GateF, Title: "Final fulfillment",
			Action:        "the owner confirms in game that the BandageDressing was at the drop point; only then may the attempt become FULFILLED",
			Preconditions: []string{"attempt VERIFICATION_REQUIRED", "Gate E clean"},
			Evidence:      []string{"named in-game observation (who, when)"},
			Rollback:      "if not found: FAILED_REVIEW, never an automatic retry"},
	}
}

// UploadStep is one step of the write sequence. Kind is READ or WRITE; only WRITE steps need a gate.
type UploadStep struct {
	N       int
	Kind    string
	Action  string
	AbortIf string
}

// UploadSequence is the ordered, verified write of one file. Nitrado has no conditional write
// (no If-Match, no version check): the sequence narrows the race to the seconds between the
// re-read (step 4) and the upload (step 5) and detects it afterwards (step 6).
func UploadSequence(target, expectBefore, expectAfter, backup string) []UploadStep {
	return []UploadStep{
		{1, "READ", "download " + target + " and compute SHA-256", "the digest is not " + expectBefore + " (or the file unexpectedly exists/does not exist)"},
		{2, "WRITE", "upload the same bytes to " + backup, "the upload fails"},
		{3, "READ", "download " + backup + " and compute SHA-256", "the digest is not " + expectBefore},
		{4, "READ", "download " + target + " again immediately before writing", "the digest changed since step 1 (someone else edited it)"},
		{5, "WRITE", "request a single-use upload token for " + target + " and POST the new content", "either request fails: go to step 6 anyway (a failed response does not prove nothing was written)"},
		{6, "READ", "download " + target + " and compute SHA-256", "the digest is neither " + expectAfter + " nor " + expectBefore + ": restore " + backup + " and verify"},
		{7, "RECORD", "record the attempt transition with both digests only after step 6 verified " + expectAfter, "the record fails: the next reconcile reads the file (Reconcile)"},
	}
}
