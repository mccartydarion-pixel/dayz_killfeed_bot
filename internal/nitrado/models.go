package nitrado

import "encoding/json"

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

// ServiceDetails contains additional server metadata that may be present in the API payload.
type ServiceDetails struct {
	ServerName  string `json:"server_name"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Hostname    string `json:"hostname"`
	Description string `json:"description"`
	Access      string `json:"access"`
}

// ServicesEnvelope is the top-level object returned by the Nitrado services API.
type ServicesEnvelope struct {
	Data []Service `json:"data"`
}

// DecodeServices decodes the service payload with tolerant parsing.
func DecodeServices(payload []byte) ([]Service, error) {
	var envelope ServicesEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		var direct []Service
		if err := json.Unmarshal(payload, &direct); err == nil {
			return direct, nil
		}
		return nil, err
	}
	return envelope.Data, nil
}
