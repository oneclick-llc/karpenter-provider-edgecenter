// Package edgecenter implements the API calls used by node provisioning.
package edgecenter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/time/rate"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var requestBudget = rate.NewLimiter(4, 1)

type Client struct {
	URL, Token      string
	HTTP            *http.Client
	Project, Region int
}
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("EdgeCenter HTTP %d: %s", e.Status, e.Message) }
func IsNotFound(err error) bool   { e, ok := err.(*APIError); return ok && e.Status == 404 }
func New(token string, project, region int) *Client {
	return &Client{URL: "https://api.edgecenter.ru/cloud", Token: token, Project: project, Region: region, HTTP: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) Path(version, resource string) string {
	return fmt.Sprintf("/%s/%s/%d/%d", version, resource, c.Project, c.Region)
}

// Do never retries a mutation. The caller records its intent before submission.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	if err := requestBudget.Wait(ctx); err != nil {
		return err
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "APIKey "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("EdgeCenter request did not return a response: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var problem struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &problem)
		if problem.Message == "" {
			problem.Message = http.StatusText(resp.StatusCode)
		}
		return &APIError{resp.StatusCode, problem.Message}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

type Task struct {
	ID      string                     `json:"id"`
	State   string                     `json:"state"`
	Error   string                     `json:"error"`
	Created map[string]json.RawMessage `json:"created_resources"`
}

func (c *Client) Task(ctx context.Context, id string) (Task, error) {
	var t Task
	err := c.Do(ctx, "GET", "/v1/tasks/"+url.PathEscape(id), nil, &t)
	return t, err
}

type TaskResponse struct {
	Tasks []string `json:"tasks"`
}

func (c *Client) Submit(ctx context.Context, path string, body any) (string, error) {
	var r TaskResponse
	err := c.Do(ctx, "POST", path, body, &r)
	if err != nil {
		return "", err
	}
	if len(r.Tasks) != 1 {
		return "", fmt.Errorf("expected one task, got %d", len(r.Tasks))
	}
	return r.Tasks[0], nil
}

type Instance struct {
	ID       string            `json:"instance_id"`
	Name     string            `json:"instance_name"`
	Status   string            `json:"status"`
	Metadata map[string]string `json:"metadata"`
	Volumes  []struct {
		ID string `json:"id"`
	} `json:"volumes"`
	Flavor struct {
		Name string `json:"flavor_name"`
		ID   string `json:"flavor_id"`
	} `json:"flavor"`
}
type Port struct {
	ID         string `json:"port_id"`
	IP         string `json:"fixed_ip_address"`
	NetworkID  string `json:"network_id"`
	InstanceID string `json:"instance_id"`
}
type Volume struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Metadata map[string]string `json:"metadata"`
	Status   string            `json:"status"`
}

func (c *Client) BareMetalCapacity(ctx context.Context) (map[string]int, error) {
	var response struct {
		Capacity map[string]int `json:"capacity"`
	}
	err := c.Do(ctx, "GET", fmt.Sprintf("/v2/bmcapacity/%d", c.Region), nil, &response)
	return response.Capacity, err
}

func (c *Client) Instance(ctx context.Context, id string) (Instance, error) {
	var v Instance
	err := c.Do(ctx, "GET", c.Path("v1", "instances")+"/"+id, nil, &v)
	return v, err
}
func List[T any](ctx context.Context, c *Client, resource string) ([]T, error) {
	var all []T
	for offset := 0; ; offset += 100 {
		var page struct {
			Count   int `json:"count"`
			Results []T `json:"results"`
		}
		path := c.Path("v1", resource) + fmt.Sprintf("?limit=100&offset=%d", offset)
		if err := c.Do(ctx, "GET", path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if len(page.Results) == 0 || len(all) >= page.Count {
			return all, nil
		}
	}
}
