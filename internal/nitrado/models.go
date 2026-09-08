package nitrado

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Service represents a flexible Nitrado service object.
type Service struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Status   string         `json:"status"`
	Game     string         `json:"game"`
	Username string         `json:"username"`
	Location string         `json:"location"`
	Details  ServiceDetails `json:"details"`
}

// UnmarshalJSON tolerates the real Nitrado payload where id is numeric and
// the game name lives under details.game.
func (s *Service) UnmarshalJSON(data []byte) error {
	type serviceAlias Service
	var wire struct {
		serviceAlias
		RawID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*s = Service(wire.serviceAlias)
	s.ID = decodeServiceID(wire.RawID)
	if s.Game == "" {
		s.Game = s.Details.Game
	}
	return nil
}

func decodeServiceID(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		return num.String()
	}
	return ""
}

// ServiceDetails contains additional server metadata that may be present in the API payload.
type ServiceDetails struct {
	ServerName  string `json:"server_name"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Hostname    string `json:"hostname"`
	Description string `json:"description"`
	Access      string `json:"access"`
	Game        string `json:"game"`
}

// ServicesEnvelope is the documented top-level object returned by GET /services.
type ServicesEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// DecodeServices decodes the service payload with tolerant parsing. Supported
// shapes: {"data":{"services":[...]}} (documented), {"data":[...]}, and a
// bare [...] array.
func DecodeServices(payload []byte) ([]Service, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty services payload")
	}

	if trimmed[0] == '[' {
		var direct []Service
		if err := json.Unmarshal(trimmed, &direct); err != nil {
			return nil, err
		}
		return direct, nil
	}

	var envelope ServicesEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, err
	}
	data := bytes.TrimSpace(envelope.Data)
	if len(data) == 0 {
		return nil, fmt.Errorf("services payload missing data")
	}

	if data[0] == '[' {
		var services []Service
		if err := json.Unmarshal(data, &services); err != nil {
			return nil, err
		}
		return services, nil
	}

	var nested struct {
		Services []Service `json:"services"`
	}
	if err := json.Unmarshal(data, &nested); err != nil {
		return nil, err
	}
	return nested.Services, nil
}
