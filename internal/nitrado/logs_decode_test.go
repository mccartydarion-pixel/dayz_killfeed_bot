package nitrado

import "testing"

func TestDecodeFileServerEntriesDocumented(t *testing.T) {
	payload := []byte(`{"status":"success","data":{"entries":[
		{"type":"dir","path":"/games/ni_1/ftproot/dayz","name":"dayz","modified_at":1757370000},
		{"type":"file","path":"/games/ni_1/ftproot/run.log","name":"run.log","size":100,"modified_at":1757370001}
	]}}`)

	entries, diag := decodeFileServerEntries(payload)
	if !diag.decodeSuccess {
		t.Fatal("expected decode_success=true")
	}
	if diag.entryLocation != "data.entries" {
		t.Fatalf("expected entryLocation=data.entries, got %q", diag.entryLocation)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestDecodeFileServerEntriesAlternateNesting(t *testing.T) {
	// Some Nitrado responses nest the array differently; we must still find it.
	payload := []byte(`{"status":"success","data":{"files":[
		{"type":"file","path":"/x/a.ADM","name":"a.ADM","size":5,"modified_at":1757370000}
	]}}`)

	entries, diag := decodeFileServerEntries(payload)
	if diag.entryLocation != "data.files" {
		t.Fatalf("expected entryLocation=data.files, got %q", diag.entryLocation)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

func TestDecodeFileServerEntriesEmptyVsParseFailure(t *testing.T) {
	// HTTP 200 but the entry array is under a key we do not map -> entries_found=0,
	// decode_success=true, entry_location=not_found. This is the diagnostic we need
	// to distinguish "empty dir" from "wrong schema".
	payload := []byte(`{"status":"success","data":{"something_else":[]}}`)

	entries, diag := decodeFileServerEntries(payload)
	if !diag.decodeSuccess {
		t.Fatal("expected decode_success=true (valid JSON)")
	}
	if diag.entryLocation != "not_found" {
		t.Fatalf("expected entryLocation=not_found, got %q", diag.entryLocation)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
	if diag.topLevelKeys == "" {
		t.Fatal("expected top-level keys to be reported for diagnosis")
	}
}
