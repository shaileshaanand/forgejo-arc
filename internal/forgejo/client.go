// Package forgejo implements the Forgejo v16 runner REST API.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Job struct {
	ID     int64    `json:"id"`
	Handle string   `json:"handle"`
	Status string   `json:"status"`
	RunsOn []string `json:"runs_on"`
}

type Runner struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	UUID        string `json:"uuid"`
	Token       string `json:"token"`
}

type Client struct {
	base, token, route string
	http               *http.Client
}

func New(base, token, scopeType, name string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("forgejoURL must be an HTTP(S) URL without credentials, query, or fragment")
	}
	parts := strings.Split(name, "/")
	if scopeType != "Repository" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("scope must be Repository with name owner/repository")
	}
	route := "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/actions/runners"
	return &Client{base: strings.TrimRight(base, "/") + "/api/v1", token: token, route: route,
		http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// APIError deliberately excludes response bodies, which may contain credentials.
type APIError struct {
	Method, Path string
	Status       int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Forgejo %s %s: HTTP %d", e.Method, e.Path, e.Status)
}

func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Forgejo request: %w", err)
	}
	defer resp.Body.Close()
	if method == http.MethodDelete && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{method, path, resp.StatusCode}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

func (c *Client) Jobs(ctx context.Context, labels []string) ([]Job, error) {
	var jobs []Job
	err := c.request(ctx, http.MethodGet, c.route+"/jobs?labels="+url.QueryEscape(strings.Join(labels, ",")), nil, &jobs)
	return jobs, err
}

func (c *Client) Register(ctx context.Context, name, description string) (Runner, error) {
	var r Runner
	err := c.request(ctx, http.MethodPost, c.route, map[string]any{"name": name, "description": description, "ephemeral": true}, &r)
	if err == nil && (r.ID == 0 || r.UUID == "" || r.Token == "") {
		err = fmt.Errorf("incomplete runner registration response")
	}
	return r, err
}

func (c *Client) Delete(ctx context.Context, id int64) error {
	return c.request(ctx, http.MethodDelete, c.route+"/"+strconv.FormatInt(id, 10), nil, nil)
}

func (c *Client) Runners(ctx context.Context) ([]Runner, error) {
	var all []Runner
	// Forgejo can cap page size below the requested limit. Read until an empty page.
	for page := 1; ; page++ {
		var runners []Runner
		err := c.request(ctx, http.MethodGet, fmt.Sprintf("%s?visible=false&limit=50&page=%d", c.route, page), nil, &runners)
		if err != nil {
			return nil, err
		}
		if len(runners) == 0 {
			return all, nil
		}
		all = append(all, runners...)
	}
}

// Matches requires every job label to be available on the runner.
func Matches(required, available []string) bool {
	if len(required) == 0 {
		return false
	}
	set := make(map[string]bool, len(available))
	for _, label := range available {
		set[label] = true
	}
	for _, label := range required {
		if !set[label] {
			return false
		}
	}
	return true
}
