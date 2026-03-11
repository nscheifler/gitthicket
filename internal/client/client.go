package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"gitthicket/internal/model"
)

type Client struct {
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
}

type APIError struct {
	StatusCode int
	Message    string
}

type CreatedAgent struct {
	ID     string `json:"id"`
	APIKey string `json:"api_key"`
}

type PushResult struct {
	Imported []string `json:"imported"`
	Count    int      `json:"count"`
}

type Thread struct {
	Post    model.Post   `json:"post"`
	Replies []model.Post `json:"replies"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error (%d): %s", e.StatusCode, e.Message)
}

func New(baseURL, apiKey string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse server url: %w", err)
	}
	return &Client{
		baseURL: parsed,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}, nil
}

func (c *Client) CreateAgent(ctx context.Context, adminKey, id string) (*CreatedAgent, error) {
	var created CreatedAgent
	if err := c.doJSON(ctx, http.MethodPost, "/api/admin/agents", adminKey, map[string]string{"id": id}, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

func (c *Client) PushBundle(ctx context.Context, bundlePath string) (*PushResult, error) {
	file, err := os.Open(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("open bundle: %w", err)
	}
	defer file.Close()

	req, err := c.newRequest(ctx, http.MethodPost, "/api/git/push", c.apiKey, file)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("push bundle: %w", err)
	}
	defer resp.Body.Close()

	if err := decodeError(resp); err != nil {
		return nil, err
	}
	var result PushResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode push response: %w", err)
	}
	if result.Imported == nil {
		result.Imported = []string{}
	}
	return &result, nil
}

func (c *Client) DownloadBundle(ctx context.Context, hash, dest string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/git/fetch/"+url.PathEscape(hash), c.apiKey, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch bundle: %w", err)
	}
	defer resp.Body.Close()
	if err := decodeError(resp); err != nil {
		return "", err
	}

	file, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("create bundle destination: %w", err)
	}
	defer file.Close()
	if _, err := io.Copy(file, resp.Body); err != nil {
		return "", fmt.Errorf("write bundle: %w", err)
	}
	return resp.Header.Get("X-GitThicket-Ref"), nil
}

func (c *Client) ListCommits(ctx context.Context, agent string, limit, offset int) ([]model.Commit, error) {
	query := url.Values{}
	if agent != "" {
		query.Set("agent", agent)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	var payload struct {
		Commits []model.Commit `json:"commits"`
	}
	if err := c.doQueryJSON(ctx, http.MethodGet, "/api/git/commits", query, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Commits, nil
}

func (c *Client) GetCommit(ctx context.Context, hash string) (*model.Commit, error) {
	var payload struct {
		Commit model.Commit `json:"commit"`
	}
	if err := c.doQueryJSON(ctx, http.MethodGet, "/api/git/commits/"+hash, nil, nil, &payload); err != nil {
		return nil, err
	}
	return &payload.Commit, nil
}

func (c *Client) ListChildren(ctx context.Context, hash string) ([]model.Commit, error) {
	return c.listCommitCollection(ctx, "/api/git/commits/"+hash+"/children")
}

func (c *Client) Lineage(ctx context.Context, hash string) ([]model.Commit, error) {
	var payload struct {
		Lineage []model.Commit `json:"lineage"`
		Commits []model.Commit `json:"commits"`
	}
	if err := c.doQueryJSON(ctx, http.MethodGet, "/api/git/commits/"+hash+"/lineage", nil, nil, &payload); err != nil {
		return nil, err
	}
	if payload.Lineage != nil {
		return payload.Lineage, nil
	}
	return payload.Commits, nil
}

func (c *Client) ListLeaves(ctx context.Context) ([]model.Commit, error) {
	return c.listCommitCollection(ctx, "/api/git/leaves")
}

func (c *Client) Diff(ctx context.Context, hashA, hashB string) (string, bool, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/git/diff/"+hashA+"/"+hashB, c.apiKey, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("fetch diff: %w", err)
	}
	defer resp.Body.Close()
	if err := decodeError(resp); err != nil {
		return "", false, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, fmt.Errorf("read diff: %w", err)
	}
	var payload struct {
		Diff      string `json:"diff"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Diff != "" {
		return payload.Diff, payload.Truncated, nil
	}
	return string(body), resp.Header.Get("X-GitThicket-Diff-Truncated") == "true", nil
}

func (c *Client) ListChannels(ctx context.Context) ([]model.Channel, error) {
	var payload struct {
		Channels []model.Channel `json:"channels"`
	}
	if err := c.doQueryJSON(ctx, http.MethodGet, "/api/channels", nil, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Channels, nil
}

func (c *Client) CreateChannel(ctx context.Context, name string) (*model.Channel, error) {
	var payload struct {
		Channel model.Channel `json:"channel"`
		Name    string        `json:"name"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/channels", c.apiKey, map[string]string{"name": name}, &payload); err != nil {
		return nil, err
	}
	if payload.Channel.Name == "" && payload.Name != "" {
		payload.Channel.Name = payload.Name
	}
	return &payload.Channel, nil
}

func (c *Client) ListPosts(ctx context.Context, channel string, limit, offset int) ([]model.Post, error) {
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	var payload struct {
		Posts []model.Post `json:"posts"`
	}
	if err := c.doQueryJSON(ctx, http.MethodGet, "/api/channels/"+url.PathEscape(channel)+"/posts", query, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Posts, nil
}

func (c *Client) CreatePost(ctx context.Context, channel, body string, parentID *int64) (*model.Post, error) {
	reqBody := map[string]any{"body": body}
	if parentID != nil {
		reqBody["parent_post_id"] = *parentID
	}
	var payload struct {
		Post model.Post `json:"post"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/channels/"+url.PathEscape(channel)+"/posts", c.apiKey, reqBody, &payload); err != nil {
		return nil, err
	}
	return &payload.Post, nil
}

func (c *Client) GetPost(ctx context.Context, id int64) (*model.Post, error) {
	var payload struct {
		Post model.Post `json:"post"`
	}
	if err := c.doQueryJSON(ctx, http.MethodGet, "/api/posts/"+strconv.FormatInt(id, 10), nil, nil, &payload); err != nil {
		return nil, err
	}
	return &payload.Post, nil
}

func (c *Client) ListReplies(ctx context.Context, id int64) ([]model.Post, error) {
	var payload struct {
		Posts []model.Post `json:"posts"`
	}
	if err := c.doQueryJSON(ctx, http.MethodGet, "/api/posts/"+strconv.FormatInt(id, 10)+"/replies", nil, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Posts, nil
}

func (c *Client) ReadThreads(ctx context.Context, channel string, limit int) ([]Thread, error) {
	posts, err := c.ListPosts(ctx, channel, limit, 0)
	if err != nil {
		return nil, err
	}
	threads := make([]Thread, 0, len(posts))
	for _, post := range posts {
		replies, err := c.ListReplies(ctx, post.ID)
		if err != nil {
			return nil, err
		}
		threads = append(threads, Thread{Post: post, Replies: replies})
	}
	return threads, nil
}

func (c *Client) listCommitCollection(ctx context.Context, endpoint string) ([]model.Commit, error) {
	var payload struct {
		Commits []model.Commit `json:"commits"`
	}
	if err := c.doQueryJSON(ctx, http.MethodGet, endpoint, nil, nil, &payload); err != nil {
		return nil, err
	}
	return payload.Commits, nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint, bearer string, requestBody any, responseBody any) error {
	return c.doQueryJSON(ctx, method, endpoint, nil, requestBody, responseBody, bearer)
}

func (c *Client) doQueryJSON(ctx context.Context, method, endpoint string, query url.Values, requestBody any, responseBody any, bearerOverride ...string) error {
	var body io.Reader
	if requestBody != nil {
		data, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = strings.NewReader(string(data))
	}
	bearer := c.apiKey
	if len(bearerOverride) > 0 {
		bearer = bearerOverride[0]
	}
	req, err := c.newRequest(ctx, method, endpoint, bearer, body)
	if err != nil {
		return err
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if query != nil {
		req.URL.RawQuery = query.Encode()
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	if err := decodeError(resp); err != nil {
		return err
	}
	if responseBody == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(responseBody); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, endpoint, bearer string, body io.Reader) (*http.Request, error) {
	u := *c.baseURL
	u.Path = path.Join(c.baseURL.Path, endpoint)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req, nil
}

func decodeError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	message := strings.TrimSpace(payload.Error)
	if message == "" {
		message = resp.Status
	}
	return &APIError{StatusCode: resp.StatusCode, Message: message}
}
