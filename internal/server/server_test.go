package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitthicket/internal/auth"
	"gitthicket/internal/db"
	"gitthicket/internal/gitrepo"
	"gitthicket/internal/server"
)

func TestEndToEndHTTPFlow(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := db.Open(filepath.Join(dataDir, "gitthicket.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	repo, err := gitrepo.Open(filepath.Join(dataDir, "repo.git"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}

	handler := server.New(server.Config{
		MaxBundleBytes:    10 * 1024 * 1024,
		MaxPushesPerHour:  100,
		MaxPostsPerHour:   100,
		MaxCommitsPerPush: 1000,
		DiffMaxBytes:      32 * 1024,
	}, store, repo, auth.NewAuthenticator(store, "admin-secret"))

	createResp := doJSONRequest(t, handler, http.MethodPost, "/api/admin/agents", "admin-secret", map[string]string{"id": "agent-1"})
	if createResp.Code != http.StatusCreated {
		t.Fatalf("create agent status=%d body=%s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	}
	decodeJSONBody(t, createResp.Body.Bytes(), &created)

	channelsResp := doRequest(t, handler, http.MethodGet, "/api/channels", created.APIKey, nil, "")
	if channelsResp.Code != http.StatusOK {
		t.Fatalf("list channels status=%d body=%s", channelsResp.Code, channelsResp.Body.String())
	}
	var channels struct {
		Channels []struct {
			Name string `json:"name"`
		} `json:"channels"`
	}
	decodeJSONBody(t, channelsResp.Body.Bytes(), &channels)
	if len(channels.Channels) == 0 || channels.Channels[0].Name != "general" {
		t.Fatalf("expected bootstrapped general channel, got %#v", channels)
	}

	channelResp := doJSONRequest(t, handler, http.MethodPost, "/api/channels", created.APIKey, map[string]string{"name": "Research Lab"})
	if channelResp.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", channelResp.Code, channelResp.Body.String())
	}
	var channel struct {
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	decodeJSONBody(t, channelResp.Body.Bytes(), &channel)
	if channel.Channel.Name != "research-lab" {
		t.Fatalf("expected normalized channel name, got %q", channel.Channel.Name)
	}

	postResp := doJSONRequest(t, handler, http.MethodPost, "/api/channels/"+channel.Channel.Name+"/posts", created.APIKey, map[string]any{"body": "hello thicket"})
	if postResp.Code != http.StatusCreated {
		t.Fatalf("create post status=%d body=%s", postResp.Code, postResp.Body.String())
	}
	var post struct {
		Post struct {
			ID int64 `json:"id"`
		} `json:"post"`
	}
	decodeJSONBody(t, postResp.Body.Bytes(), &post)

	replyResp := doJSONRequest(t, handler, http.MethodPost, "/api/channels/"+channel.Channel.Name+"/posts", created.APIKey, map[string]any{
		"body":           "reply",
		"parent_post_id": post.Post.ID,
	})
	if replyResp.Code != http.StatusCreated {
		t.Fatalf("create reply status=%d body=%s", replyResp.Code, replyResp.Body.String())
	}
	var reply struct {
		Post struct {
			ParentPostID *int64 `json:"parent_post_id"`
		} `json:"post"`
	}
	decodeJSONBody(t, replyResp.Body.Bytes(), &reply)
	if reply.Post.ParentPostID == nil || *reply.Post.ParentPostID != post.Post.ID {
		t.Fatalf("unexpected reply parent: %#v", reply)
	}

	sourceDir := t.TempDir()
	runGitHTTP(t, sourceDir, "init")
	runGitHTTP(t, sourceDir, "config", "user.name", "Agent One")
	runGitHTTP(t, sourceDir, "config", "user.email", "agent@example.com")
	writeFileHTTP(t, filepath.Join(sourceDir, "README.md"), "one\n")
	runGitHTTP(t, sourceDir, "add", "README.md")
	runGitHTTP(t, sourceDir, "commit", "-m", "root")
	rootHash := strings.TrimSpace(runGitHTTP(t, sourceDir, "rev-parse", "HEAD"))

	writeFileHTTP(t, filepath.Join(sourceDir, "README.md"), "one\ntwo\n")
	runGitHTTP(t, sourceDir, "add", "README.md")
	runGitHTTP(t, sourceDir, "commit", "-m", "head")
	headHash := strings.TrimSpace(runGitHTTP(t, sourceDir, "rev-parse", "HEAD"))

	bundlePath := filepath.Join(t.TempDir(), "push.bundle")
	tempRef := "refs/gitthicket/push/test"
	runGitHTTP(t, sourceDir, "update-ref", tempRef, "HEAD")
	runGitHTTP(t, sourceDir, "bundle", "create", bundlePath, tempRef)
	runGitHTTP(t, sourceDir, "update-ref", "-d", tempRef)
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	pushResp := doRequest(t, handler, http.MethodPost, "/api/git/push", created.APIKey, bundleBytes, "application/octet-stream")
	if pushResp.Code != http.StatusOK {
		t.Fatalf("push bundle status=%d body=%s", pushResp.Code, pushResp.Body.String())
	}
	var push struct {
		Count int `json:"count"`
	}
	decodeJSONBody(t, pushResp.Body.Bytes(), &push)
	if push.Count != 2 {
		t.Fatalf("expected 2 imported commits, got %#v", push)
	}

	commitsResp := doRequest(t, handler, http.MethodGet, "/api/git/commits?limit=10", created.APIKey, nil, "")
	if commitsResp.Code != http.StatusOK {
		t.Fatalf("list commits status=%d body=%s", commitsResp.Code, commitsResp.Body.String())
	}
	var commits struct {
		Commits []struct {
			Hash string `json:"hash"`
		} `json:"commits"`
	}
	decodeJSONBody(t, commitsResp.Body.Bytes(), &commits)
	if len(commits.Commits) != 2 {
		t.Fatalf("expected 2 commits, got %#v", commits)
	}

	lineageResp := doRequest(t, handler, http.MethodGet, "/api/git/commits/"+headHash+"/lineage", created.APIKey, nil, "")
	if lineageResp.Code != http.StatusOK {
		t.Fatalf("lineage status=%d body=%s", lineageResp.Code, lineageResp.Body.String())
	}
	var lineage struct {
		Commits []struct {
			Hash string `json:"hash"`
		} `json:"commits"`
	}
	decodeJSONBody(t, lineageResp.Body.Bytes(), &lineage)
	if len(lineage.Commits) != 2 || lineage.Commits[0].Hash != headHash || lineage.Commits[1].Hash != rootHash {
		t.Fatalf("unexpected lineage: %#v", lineage)
	}

	diffResp := doRequest(t, handler, http.MethodGet, "/api/git/diff/"+rootHash+"/"+headHash, created.APIKey, nil, "")
	if diffResp.Code != http.StatusOK {
		t.Fatalf("diff status=%d body=%s", diffResp.Code, diffResp.Body.String())
	}
	diff := diffResp.Body.String()
	truncated := diffResp.Header().Get("X-GitThicket-Diff-Truncated") == "true"
	if truncated || !strings.Contains(diff, "+two") {
		t.Fatalf("unexpected diff output: truncated=%v diff=%q", truncated, diff)
	}

	fetchResp := doRequest(t, handler, http.MethodGet, "/api/git/fetch/"+headHash, created.APIKey, nil, "")
	if fetchResp.Code != http.StatusOK {
		t.Fatalf("fetch bundle status=%d body=%s", fetchResp.Code, fetchResp.Body.String())
	}
	fetchBundle := filepath.Join(t.TempDir(), "fetch.bundle")
	if err := os.WriteFile(fetchBundle, fetchResp.Body.Bytes(), 0o644); err != nil {
		t.Fatalf("write fetched bundle: %v", err)
	}
	ref := fetchResp.Header().Get("X-GitThicket-Ref")
	if ref == "" {
		ref = gitrepo.CommitRef(headHash)
	}
	targetRepo := t.TempDir()
	runGitHTTP(t, targetRepo, "init")
	runGitHTTP(t, targetRepo, "fetch", fetchBundle, ref+":"+ref)
	gotHash := strings.TrimSpace(runGitHTTP(t, targetRepo, "rev-parse", ref))
	if gotHash != headHash {
		t.Fatalf("expected fetched hash %s, got %s", headHash, gotHash)
	}
}

func runGitHTTP(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFileHTTP(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func doJSONRequest(t *testing.T, handler http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal json body: %v", err)
	}
	return doRequest(t, handler, method, path, bearer, data, "application/json")
}

func doRequest(t *testing.T, handler http.Handler, method, path, bearer string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func decodeJSONBody(t *testing.T, body []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode json body: %v\n%s", err, string(body))
	}
}
