package nitrado

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.nitrado.net"

// ErrorKind classifies a failed Nitrado request so callers can distinguish
// authentication failures from endpoint, lookup, permission, and temporary errors.
type ErrorKind string

const (
	KindAuthentication  ErrorKind = "authentication"
	KindPermission      ErrorKind = "permission"
	KindNotFound        ErrorKind = "not_found"
	KindInvalidEndpoint ErrorKind = "invalid_endpoint"
	KindTemporary       ErrorKind = "temporary"
	KindUnknown         ErrorKind = "unknown"
)

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

// BaseURL returns the configured API base URL (safe to log; not a credential).
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
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

	// Log only the method and sanitized path. Never log the token or any header.
	slog.Info("component=nitrado", "method", method, "path", path)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &RequestError{Op: "request", Kind: KindTemporary, Message: err.Error(), StatusCode: 0}
	}

	return resp, nil
}

// RequestError wraps API request failures in a useful form.
type RequestError struct {
	Op         string
	Kind       ErrorKind
	Message    string
	StatusCode int
}

func (e *RequestError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("%s failed: status=%d kind=%s message=%s", e.Op, e.StatusCode, e.Kind, e.Message)
	}
	return fmt.Sprintf("%s failed: kind=%s message=%s", e.Op, e.Kind, e.Message)
}

// classifyStatus maps an HTTP status to a failure kind for a given operation.
// notFoundKind distinguishes a wrong base path (invalid endpoint) from a
// missing service (not found) since both arrive as HTTP 404.
func classifyStatus(op string, statusCode int, notFoundKind ErrorKind) *RequestError {
	switch {
	case statusCode == http.StatusUnauthorized:
		return &RequestError{Op: op, Kind: KindAuthentication, Message: "invalid or expired access token", StatusCode: statusCode}
	case statusCode == http.StatusForbidden:
		return &RequestError{Op: op, Kind: KindPermission, Message: "permission denied for token scope", StatusCode: statusCode}
	case statusCode == http.StatusNotFound:
		return &RequestError{Op: op, Kind: notFoundKind, Message: "not found", StatusCode: statusCode}
	case statusCode == http.StatusTooManyRequests:
		return &RequestError{Op: op, Kind: KindTemporary, Message: "rate limited", StatusCode: statusCode}
	case statusCode >= http.StatusInternalServerError:
		return &RequestError{Op: op, Kind: KindTemporary, Message: "temporary Nitrado failure", StatusCode: statusCode}
	default:
		return &RequestError{Op: op, Kind: KindUnknown, Message: "unexpected status", StatusCode: statusCode}
	}
}

// AuthenticationCheck verifies the configured token against the documented
// service list endpoint (GET /services). A 401 means authentication failed;
// a 404 here means the endpoint path itself is wrong, not the token.
func (c *Client) AuthenticationCheck(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/services", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return classifyStatus("authentication", resp.StatusCode, KindInvalidEndpoint)
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
