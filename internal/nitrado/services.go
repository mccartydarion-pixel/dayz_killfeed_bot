package nitrado

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// GetServices fetches the Nitrado service list for the authenticated account.
// Uses the documented endpoint GET /services (no /v1 prefix exists).
func (c *Client) GetServices(ctx context.Context) ([]Service, error) {
	resp, err := c.do(ctx, http.MethodGet, "/services", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, classifyStatus("service list", resp.StatusCode, KindInvalidEndpoint)
	}

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read services body: %w", err)
	}

	services, err := DecodeServices(payload)
	if err != nil {
		return nil, fmt.Errorf("decode services: %w", err)
	}

	slog.Info("component=nitrado", "msg", "authentication successful")
	return services, nil
}

// FindDayZServices filters the service list for DayZ-like entries.
// Real payloads carry names like "DayZ (PS4)", so matching is a substring check.
func FindDayZServices(services []Service) []Service {
	matches := make([]Service, 0, len(services))
	for _, service := range services {
		if strings.Contains(strings.ToLower(service.Game), "dayz") || strings.Contains(strings.ToLower(service.Type), "dayz") || strings.Contains(strings.ToLower(service.Details.Type), "dayz") {
			matches = append(matches, service)
		}
	}
	return matches
}

// ValidateServiceID ensures that the configured service exists and is plausibly a DayZ server.
// It returns the matched service so callers can log the real game/type/status values.
func (c *Client) ValidateServiceID(ctx context.Context, serviceID string, services []Service) (*Service, error) {
	for i := range services {
		service := services[i]
		if service.ID == serviceID {
			// Real payloads use names like "DayZ (PS4)"; match by substring.
			if !strings.Contains(strings.ToLower(service.Game), "dayz") {
				slog.Warn("component=nitrado", "msg", "configured service does not appear to be DayZ", "service_id", serviceID, "game", service.Game)
			} else {
				slog.Info("component=nitrado", "msg", "DayZ service discovered", "service_id", serviceID)
			}
			return &service, nil
		}
	}
	return nil, fmt.Errorf("configured Nitrado service ID was not found")
}
