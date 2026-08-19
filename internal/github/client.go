// Package github files and maintains GitHub issues for violations. Issues
// are deduplicated across CI runs by a hidden marker comment in the body, so
// repeated runs never file duplicates; fixed violations close their issues.
package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal GitHub REST client — plain HTTP so the binary works in
// any runner without the gh CLI.
type Client struct {
	Token   string
	Repo    string // "org/repo"
	APIBase string // overridable for tests; default https://api.github.com
	HTTP    *http.Client
}

func NewClient(token, repo string) *Client {
	return &Client{
		Token:   token,
		Repo:    repo,
		APIBase: "https://api.github.com",
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Issue is the subset of the API's issue object we consume.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.APIBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api: %d: %s", e.Status, truncate(e.Body, 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ListLabeled returns all issues (open and closed) carrying the label.
// Closed issues matter: a recurring violation reopens its old issue instead
// of filing a duplicate.
func (c *Client) ListLabeled(label string) ([]Issue, error) {
	var all []Issue
	for page := 1; ; page++ {
		var batch []Issue
		path := fmt.Sprintf("/repos/%s/issues?labels=%s&state=all&per_page=100&page=%d",
			c.Repo, label, page)
		if err := c.do("GET", path, nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

// EnsureLabel creates the label if missing; an already-exists response is
// success.
func (c *Client) EnsureLabel(name, color, description string) error {
	err := c.do("POST", "/repos/"+c.Repo+"/labels", map[string]string{
		"name": name, "color": color, "description": description,
	}, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == 422 &&
		strings.Contains(apiErr.Body, "already_exists") {
		return nil
	}
	return err
}

func (c *Client) CreateIssue(title, body string, labels []string) (int, error) {
	var created Issue
	err := c.do("POST", "/repos/"+c.Repo+"/issues", map[string]any{
		"title": title, "body": body, "labels": labels,
	}, &created)
	return created.Number, err
}

func (c *Client) Comment(number int, body string) error {
	return c.do("POST", fmt.Sprintf("/repos/%s/issues/%d/comments", c.Repo, number),
		map[string]string{"body": body}, nil)
}

func (c *Client) SetState(number int, state string) error {
	return c.do("PATCH", fmt.Sprintf("/repos/%s/issues/%d", c.Repo, number),
		map[string]string{"state": state}, nil)
}
