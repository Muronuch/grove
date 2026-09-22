package routerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Muronuch/grove/internal/meta"
	"github.com/Muronuch/grove/internal/router"
)

var ErrUnreachable = errors.New("the " + meta.Name + " router is not reachable")

var ErrStaleTable = errors.New("the router holds a newer route table")

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(adminPort int, token string) *Client {
	return &Client{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", adminPort),
		Token:   token,
		HTTP: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 4,
			},
		},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set("authorization", "Bearer "+c.Token)

	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusConflict {
		return ErrStaleTable
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return fmt.Errorf("router %s %s: %s: %s", method, path, res.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (c *Client) Health(ctx context.Context) (router.HealthResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var h router.HealthResponse
	err := c.do(ctx, http.MethodGet, "/v1/health", nil, &h)
	return h, err
}

func (c *Client) PutRoutes(ctx context.Context, t router.Table) error {
	return c.do(ctx, http.MethodPut, "/v1/routes", t, nil)
}

func (c *Client) Routes(ctx context.Context) (router.Table, error) {
	var t router.Table
	err := c.do(ctx, http.MethodGet, "/v1/routes", nil, &t)
	return t, err
}

func (c *Client) SetState(ctx context.Context, project, env string, state router.EnvState) error {
	return c.do(ctx, http.MethodPatch, "/v1/routes/state", map[string]any{
		"project": project, "env": env, "state": state,
	}, nil)
}

func (c *Client) Activity(ctx context.Context) (map[string]router.ActivityEntry, error) {
	var out map[string]router.ActivityEntry
	err := c.do(ctx, http.MethodGet, "/v1/activity", nil, &out)
	return out, err
}

type WakeRequest struct {
	Project string `json:"project"`
	Env     string `json:"env"`
	Browser bool   `json:"browser"`
}

func (c *Client) Wake(ctx context.Context) ([]WakeRequest, error) {
	poll := *c
	poll.HTTP = &http.Client{Timeout: 45 * time.Second, Transport: c.HTTP.Transport}
	var out struct {
		Wake []WakeRequest `json:"wake"`
	}
	if err := poll.do(ctx, http.MethodGet, "/v1/wake", nil, &out); err != nil {
		return nil, err
	}
	return out.Wake, nil
}

func (c *Client) Woke(ctx context.Context, project, env string) error {
	return c.do(ctx, http.MethodPost, "/v1/woke", map[string]any{
		"project": project, "env": env,
	}, nil)
}
