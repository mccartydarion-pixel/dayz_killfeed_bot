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
func (c *Client) GetServices(ctx context.Context) ([]Service, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/services", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &RequestError{Op: "nitrado services", Message: "unauthorized", StatusCode: http.StatusUnauthorized}
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, &RequestError{Op: "nitrado services", Message: "forbidden", StatusCode: http.StatusForbidden}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, &RequestError{Op: "nitrado services", Message: "not found", StatusCode: http.StatusNotFound}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RequestError{Op: "nitrado services", Message: "rate limited", StatusCode: http.StatusTooManyRequests}
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return nil, &RequestError{Op: "nitrado services", Message: fmt.Sprintf("status=%d", resp.StatusCode), StatusCode: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &RequestError{Op: "nitrado services", Message: fmt.Sprintf("status=%d", resp.StatusCode), StatusCode: resp.StatusCode}
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
func FindDayZServices(services []Service) []Service {
	matches := make([]Service, 0, len(services))
	for _, service := range services {
		if strings.EqualFold(service.Game, "DayZ") || strings.Contains(strings.ToLower(service.Type), "dayz") || strings.Contains(strings.ToLower(service.Details.Type), "dayz") {
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
			if !strings.EqualFold(service.Game, "DayZ") {
				slog.Warn("component=nitrado", "msg", "configured service does not appear to be DayZ", "service_id", serviceID, "game", service.Game)
			} else {
				slog.Info("component=nitrado", "msg", "DayZ service discovered", "service_id", serviceID)
			}
			return &service, nil
		}
	}
	return nil, fmt.Errorf("configured Nitrado service ID was not found")
}
