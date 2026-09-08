package nitrado

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.nitrado.net"

// Client is the reusable Nitrado HTTP client.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewClient(baseURL, token string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, httpClient: hc}
}

func (c *Client) do(ctx context.Context, method string, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &RequestError{Op: "request", Message: err.Error(), StatusCode: 0}
	}

	return resp, nil
}

// RequestError wraps API request failures in a useful form.
type RequestError struct {
	Op         string
	Message    string
	StatusCode int
}

func (e *RequestError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("%s failed: status=%d message=%s", e.Op, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s failed: %s", e.Op, e.Message)
}

// AuthenticationCheck verifies the configured token is accepted by the Nitrado API.
func (c *Client) AuthenticationCheck(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/v1/services", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return &RequestError{Op: "nitrado auth", Message: "unauthorized", StatusCode: http.StatusUnauthorized}
	}
	if resp.StatusCode == http.StatusForbidden {
		return &RequestError{Op: "nitrado auth", Message: "forbidden", StatusCode: http.StatusForbidden}
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return &RequestError{Op: "nitrado auth", Message: fmt.Sprintf("status=%d", resp.StatusCode), StatusCode: resp.StatusCode}
	}

	return nil
}

// decodeJSON is a helper for API response parsing.
func decodeJSON[T any](body io.Reader) (T, error) {
	var zero T
	if body == nil {
		return zero, fmt.Errorf("empty response body")
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return zero, fmt.Errorf("read response body: %w", err)
	}
	if len(data) == 0 {
		return zero, fmt.Errorf("empty response body")
	}
	if err := json.Unmarshal(data, &zero); err != nil {
		return zero, fmt.Errorf("decode JSON: %w", err)
	}
	return zero, nil
}
