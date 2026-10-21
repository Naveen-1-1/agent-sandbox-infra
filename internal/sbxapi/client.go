package sbxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client calls the gateway's REST API.
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8080
	Token   string
	HTTP    *http.Client
}

// StatusError is a non-2xx response.
type StatusError struct {
	Status int
	Body   Error
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("gateway: %d %s: %s", e.Status, e.Body.Code, e.Body.Error)
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func path(ns, name string, parts ...string) string {
	p := "/api/v1/workspaces/" + url.PathEscape(ns) + "/" + url.PathEscape(name)
	for _, s := range parts {
		p += "/" + s
	}
	return p
}

func (c *Client) do(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+p, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		se := &StatusError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, &se.Body)
		return se
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) Create(ctx context.Context, r CreateRequest) (*CreateResponse, error) {
	out := &CreateResponse{}
	return out, c.do(ctx, http.MethodPost, "/api/v1/workspaces", r, out)
}

func (c *Client) List(ctx context.Context) ([]Workspace, error) {
	var out []Workspace
	return out, c.do(ctx, http.MethodGet, "/api/v1/workspaces", nil, &out)
}

func (c *Client) Get(ctx context.Context, ns, name string) (*Workspace, error) {
	out := &Workspace{}
	return out, c.do(ctx, http.MethodGet, path(ns, name), nil, out)
}

func (c *Client) Delete(ctx context.Context, ns, name string) error {
	return c.do(ctx, http.MethodDelete, path(ns, name), nil, nil)
}

func (c *Client) WorkspaceToken(ctx context.Context, ns, name string) (string, error) {
	out := &TokenResponse{}
	return out.Token, c.do(ctx, http.MethodGet, path(ns, name, "token"), nil, out)
}

func (c *Client) Exec(ctx context.Context, ns, name string, r ExecRequest) (*ExecResponse, error) {
	out := &ExecResponse{}
	return out, c.do(ctx, http.MethodPost, path(ns, name, "exec"), r, out)
}

func (c *Client) Wake(ctx context.Context, ns, name string) (*Workspace, error) {
	out := &Workspace{}
	return out, c.do(ctx, http.MethodPost, path(ns, name, "wake"), nil, out)
}

func (c *Client) WakeHint(ctx context.Context, ns, name string) error {
	return c.do(ctx, http.MethodPost, path(ns, name, "wake-hint"), nil, nil)
}

func (c *Client) TurnEnded(ctx context.Context, ns, name string) error {
	return c.do(ctx, http.MethodPost, path(ns, name, "turn-ended"), nil, nil)
}

func (c *Client) Suspend(ctx context.Context, ns, name string) error {
	return c.do(ctx, http.MethodPost, path(ns, name, "suspend"), nil, nil)
}

// EditorURL returns a sign-in link to the editor (port 0) or an app port.
func (c *Client) EditorURL(ctx context.Context, ns, name string, port int) (string, error) {
	out := &EditorResponse{}
	return out.URL, c.do(ctx, http.MethodPost, path(ns, name, "editor"), EditorRequest{Port: port}, out)
}

// Services lists the declared services and, if the workspace is awake,
// their state. It never wakes the workspace.
func (c *Client) Services(ctx context.Context, ns, name string) ([]Service, error) {
	var out []Service
	return out, c.do(ctx, http.MethodGet, path(ns, name, "services"), nil, &out)
}

// PutService declares (or redeclares) a service. It starts now if the
// workspace is awake, and after every wake.
func (c *Client) PutService(ctx context.Context, ns, name string, svc Service) error {
	return c.do(ctx, http.MethodPut, path(ns, name, "services", svc.Name), svc, nil)
}

// DeleteService stops a service and removes its declaration.
func (c *Client) DeleteService(ctx context.Context, ns, name, svc string) error {
	return c.do(ctx, http.MethodDelete, path(ns, name, "services", svc), nil, nil)
}

// ReadFile returns a workspace file's contents.
func (c *Client) ReadFile(ctx context.Context, ns, name, file string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.BaseURL, "/")+path(ns, name, "files", strings.TrimPrefix(file, "/")), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		se := &StatusError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, &se.Body)
		return nil, se
	}
	return data, err
}

// WriteFile replaces a workspace file.
func (c *Client) WriteFile(ctx context.Context, ns, name, file string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimSuffix(c.BaseURL, "/")+path(ns, name, "files", strings.TrimPrefix(file, "/")), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		se := &StatusError{Status: resp.StatusCode}
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &se.Body)
		return se
	}
	return nil
}
