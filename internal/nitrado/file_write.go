package nitrado

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// File-server WRITE primitives (Champion Shop Gate A tool, docs/SHOP_GATE_A_UPLOAD.md). They are
// called ONLY by internal/shop/missionwrite, which wraps every write in an owner-authorized, fully
// re-verified single operation; the bot's startup, Live Sync and Shop delivery never call them
// (enforced by missionwrite's import test).
//
// Protocol, from Nitrado's official PHP SDK (github.com/nitrado/NitrAPI-PHP,
// lib/Nitrapi/Services/Gameservers/FileServer/FileServer.php: uploadToken / writeFile / createDirectory):
//
//  1. POST /services/{id}/gameservers/file_server/upload, form fields path=<directory>, file=<name>
//     -> {"data":{"token":{"url":"<file server URL>","token":"<single-use token>"}}}
//  2. POST <url> with header "token: <token>", Content-Type application/binary, body = the raw bytes.
//     The SDK documents that an existing file is overwritten.
//
//  mkdir: POST /services/{id}/gameservers/file_server/mkdir, form fields path=<parent>, name=<dir>.
//
// None of this is retried: a write is attempted at most once per call, and the caller decides what
// happened by reading the file back. The upload URL and token are credentials: they are never
// logged, never returned in an error and never printed (UploadTarget redacts itself).

// UploadTarget is the single-use destination from step 1. Its fields are unexported so no caller can
// log them by accident; String, GoString and MarshalJSON all redact.
type UploadTarget struct {
	url   string
	token string
}

func (UploadTarget) String() string               { return "UploadTarget{<redacted>}" }
func (UploadTarget) GoString() string             { return "UploadTarget{<redacted>}" }
func (UploadTarget) MarshalJSON() ([]byte, error) { return []byte(`"<redacted>"`), nil }
func (t UploadTarget) Valid() bool                { return t.url != "" && t.token != "" }

// WritePhase names the step a write error came from.
type WritePhase string

const (
	PhaseToken    WritePhase = "upload_token" // step 1: nothing can have been written
	PhaseTransfer WritePhase = "transfer"     // step 2: the file may or may not have been written
	PhaseMkdir    WritePhase = "mkdir"
)

// WriteError is a sanitized write failure: an operation label, a phase, a kind and the HTTP status.
// It never carries a URL, a token or a response body.
type WriteError struct {
	Phase      WritePhase
	Kind       ErrorKind
	StatusCode int
	Detail     string // fixed vocabulary only (e.g. "network", "no token in response")
}

func (e *WriteError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("nitrado %s failed: status=%d kind=%s %s", e.Phase, e.StatusCode, e.Kind, e.Detail)
	}
	return fmt.Sprintf("nitrado %s failed: kind=%s %s", e.Phase, e.Kind, e.Detail)
}

// ErrInsecureUploadURL: the upload destination is not an https URL with a host. The token is never
// sent to it.
var ErrInsecureUploadURL = errors.New("nitrado: upload destination is not an https URL")

// AllowInsecureUploadURL permits http upload destinations. Only the disposable test stand-in sets it.
var AllowInsecureUploadURL = false

// DirEntry is one file-server list entry, directories included (ListDir returns files only).
type DirEntry struct {
	Name  string
	Path  string
	IsDir bool
	Size  int64
}

// ListEntries lists one directory (files and directories, no recursion). A read.
func (c *Client) ListEntries(ctx context.Context, serviceID, dir string) ([]DirEntry, error) {
	if serviceID == "" || dir == "" {
		return nil, fmt.Errorf("service ID and directory are required")
	}
	entries, err := c.listFileServerDir(ctx, serviceID, dir)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		p := e.Path
		if p == "" {
			p = joinRemotePath(dir, e.Name)
		}
		out = append(out, DirEntry{Name: e.Name, Path: p, IsDir: e.Type == "dir" || e.Type == "directory", Size: e.Size})
	}
	return out, nil
}

// RequestUploadToken is step 1. dir is the full file-server directory, name the file name only.
// Any failure here means nothing was written.
func (c *Client) RequestUploadToken(ctx context.Context, serviceID, dir, name string) (UploadTarget, error) {
	if serviceID == "" || dir == "" || name == "" || strings.ContainsAny(name, "/\\\x00") {
		return UploadTarget{}, &WriteError{Phase: PhaseToken, Kind: KindUnknown, Detail: "invalid arguments"}
	}
	form := url.Values{"path": {dir}, "file": {name}}
	body, status, err := c.postFormOnce(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers/file_server/upload", form)
	if err != nil {
		return UploadTarget{}, &WriteError{Phase: PhaseToken, Kind: KindTemporary, Detail: "network"}
	}
	if status != http.StatusOK && status != http.StatusCreated {
		re := classifyStatus("upload token", status, KindNotFound)
		return UploadTarget{}, &WriteError{Phase: PhaseToken, Kind: re.Kind, StatusCode: status, Detail: re.Message}
	}
	var env struct {
		Data struct {
			Token struct {
				URL   string `json:"url"`
				Token string `json:"token"`
			} `json:"token"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil || env.Data.Token.URL == "" || env.Data.Token.Token == "" {
		return UploadTarget{}, &WriteError{Phase: PhaseToken, Kind: KindUnknown, StatusCode: status, Detail: "no token in response"}
	}
	t := UploadTarget{url: env.Data.Token.URL, token: env.Data.Token.Token}
	if err := checkUploadURL(t.url); err != nil {
		return UploadTarget{}, err
	}
	return t, nil
}

func checkUploadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return ErrInsecureUploadURL
	}
	if u.Scheme == "https" || (u.Scheme == "http" && AllowInsecureUploadURL) {
		return nil
	}
	return ErrInsecureUploadURL
}

// PostUpload is step 2: exactly one POST of data to the target. It does not follow redirects (the
// token header would travel with them). A nil error means the file server answered 2xx; it is not
// proof - callers read the file back. A non-nil error does not prove nothing was written either.
func (c *Client) PostUpload(ctx context.Context, t UploadTarget, data []byte) error {
	if !t.Valid() {
		return &WriteError{Phase: PhaseTransfer, Kind: KindUnknown, Detail: "invalid upload target"}
	}
	if err := checkUploadURL(t.url); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(data))
	if err != nil {
		return &WriteError{Phase: PhaseTransfer, Kind: KindUnknown, Detail: "request"}
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/binary")
	req.Header.Set("token", t.token)
	resp, err := c.noRedirect().Do(req)
	if err != nil {
		return &WriteError{Phase: PhaseTransfer, Kind: KindTemporary, Detail: "network"}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		kind := KindUnknown
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			kind = KindAuthentication
		case resp.StatusCode == http.StatusForbidden:
			kind = KindPermission
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			kind = KindTemporary
		}
		return &WriteError{Phase: PhaseTransfer, Kind: kind, StatusCode: resp.StatusCode, Detail: "file server refused"}
	}
	return nil
}

// Mkdir creates ONE directory (name) inside an existing parent. Not recursive.
func (c *Client) Mkdir(ctx context.Context, serviceID, parent, name string) error {
	if serviceID == "" || parent == "" || name == "" || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return &WriteError{Phase: PhaseMkdir, Kind: KindUnknown, Detail: "invalid arguments"}
	}
	_, status, err := c.postFormOnce(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers/file_server/mkdir", url.Values{"path": {parent}, "name": {name}})
	if err != nil {
		return &WriteError{Phase: PhaseMkdir, Kind: KindTemporary, Detail: "network"}
	}
	if status != http.StatusOK && status != http.StatusCreated {
		re := classifyStatus("mkdir", status, KindNotFound)
		return &WriteError{Phase: PhaseMkdir, Kind: re.Kind, StatusCode: status, Detail: re.Message}
	}
	return nil
}

// postFormOnce sends one form-encoded POST to the API (no retry: a write must not repeat itself).
// The returned error is only ever a transport failure and is not surfaced verbatim by callers.
func (c *Client) postFormOnce(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.noRedirect().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func (c *Client) noRedirect() *http.Client {
	hc := *c.httpClient
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}
