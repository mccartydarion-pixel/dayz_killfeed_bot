package app

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadCaseBaseJSONRequiresOneBoundedObject(t *testing.T) {
	valid := `{"ownerPlayerId":1,"mapKey":"chernarusplus","name":"North","centerX":100,"centerZ":200,"radius":50}`
	tests := []struct {
		name, body string
		wantError  bool
	}{
		{"valid", valid, false},
		{"whitespace", valid + " \n\t", false},
		{"appended", valid + `{"state":"REVIEWED"}`, true},
		{"unknown", `{"ownerPlayerId":1,"state":"REVIEWED"}`, true},
		{"truncated", `{"ownerPlayerId":1`, true},
		{"empty", "", true},
		{"oversized", valid + strings.Repeat(" ", 4096), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/case/bases", strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			var draft caseCreateBaseDraftRequest
			err := readCaseBaseJSON(rr, req, &draft)
			if (err != nil) != tt.wantError {
				t.Fatalf("error=%v wantError=%v", err, tt.wantError)
			}
			if err == nil && (draft.OwnerPlayerID != 1 || draft.MapKey != "chernarusplus") {
				t.Fatalf("incorrect decoded draft: %+v", draft)
			}
		})
	}
}

func TestReadCaseBaseGrantRejectsAmbiguousAppendedDocument(t *testing.T) {
	req := httptest.NewRequest("POST", "/case/bases/1/grants", strings.NewReader(`{"playerId":3}{"factionId":5}`))
	var grant caseAddGrantRequest
	if err := readCaseBaseJSON(httptest.NewRecorder(), req, &grant); err == nil {
		t.Fatal("appended grant document accepted")
	}
}
