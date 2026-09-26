package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// An in-flight or possibly spawned automatic delivery is a 409 with a stable code, never a 500.
func TestShopFailedMapsDeliveryAttemptActive(t *testing.T) {
	rr := httptest.NewRecorder()
	shopFailed(rr, "refund shop purchase", fmt.Errorf("refund: %w", repository.ErrShopDeliveryAttemptActive))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d", rr.Code)
	}
	var body apiErrorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error.Code != "DELIVERY_ATTEMPT_ACTIVE" || body.Error.Message == "" {
		t.Fatalf("%s %v", rr.Body.String(), err)
	}
}
