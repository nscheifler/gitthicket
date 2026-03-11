package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitthicket/internal/auth"
	"gitthicket/internal/db"
	"gitthicket/internal/gitrepo"
	"gitthicket/internal/model"
	"gitthicket/internal/server"
)

func TestEndToEndHTTPFlow(t *testing.T) {
	t.Parallel()

	repo, _, handler, created := newTestServerWithAgent(t)
	_ = repo

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

	_, bundlePath, rootHash, headHash := makeCommitBundle(t)
	pushResp := doRequest(t, handler, http.MethodPost, "/api/git/push", created.APIKey, mustReadFile(t, bundlePath), "application/octet-stream")
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

func TestAuthFailuresReturnJSON(t *testing.T) {
	t.Parallel()

	_, store, handler, created := newTestServerWithAgent(t)

	missingResp := doRequest(t, handler, http.MethodGet, "/api/channels", "", nil, "")
	assertJSONError(t, missingResp, http.StatusUnauthorized, "missing bearer token")

	malformedResp := doRawRequest(t, handler, http.MethodGet, "/api/channels", "Token nope", nil, "")
	assertJSONError(t, malformedResp, http.StatusUnauthorized, "malformed bearer token")

	adminResp := doRequest(t, handler, http.MethodPost, "/api/admin/agents", created.APIKey, []byte(`{"id":"x"}`), "application/json")
	assertJSONError(t, adminResp, http.StatusUnauthorized, "invalid admin key")

	if _, err := store.CreateAgent(context.Background(), "agent-disabled", auth.HashAPIKey("disabled-key")); err != nil {
		t.Fatalf("create disabled agent: %v", err)
	}
	if err := store.DisableAgent(context.Background(), "agent-disabled", time.Now().UTC()); err != nil {
		t.Fatalf("disable agent: %v", err)
	}
	disabledResp := doRequest(t, handler, http.MethodGet, "/api/channels", "disabled-key", nil, "")
	assertJSONError(t, disabledResp, http.StatusUnauthorized, "disabled agent key")
}

func TestRateLimitRejection(t *testing.T) {
	t.Parallel()

	_, _, handler, created := newTestServerWithAgent(t, func(cfg *server.Config) {
		cfg.MaxPostsPerHour = 1
	})

	first := doJSONRequest(t, handler, http.MethodPost, "/api/channels/general/posts", created.APIKey, map[string]any{"body": "first"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first post status=%d body=%s", first.Code, first.Body.String())
	}

	second := doJSONRequest(t, handler, http.MethodPost, "/api/channels/general/posts", created.APIKey, map[string]any{"body": "second"})
	assertJSONError(t, second, http.StatusTooManyRequests, "rate limit exceeded for post")
	if second.Header().Get("Retry-After") == "" {
		t.Fatalf("expected retry-after header on rate limit response")
	}
}

func TestPushRollbackRemovesOnlyNewRefs(t *testing.T) {
	t.Parallel()

	repo, _, handler, created := newTestServerWithAgent(t)

	sourceDir := t.TempDir()
	runGitHTTP(t, sourceDir, "init")
	runGitHTTP(t, sourceDir, "config", "user.name", "Agent One")
	runGitHTTP(t, sourceDir, "config", "user.email", "agent@example.com")
	writeFileHTTP(t, filepath.Join(sourceDir, "README.md"), "one\n")
	runGitHTTP(t, sourceDir, "add", "README.md")
	runGitHTTP(t, sourceDir, "commit", "-m", "root")
	rootHash := strings.TrimSpace(runGitHTTP(t, sourceDir, "rev-parse", "HEAD"))
	bundlePath1 := filepath.Join(t.TempDir(), "push-1.bundle")
	runGitHTTP(t, sourceDir, "bundle", "create", bundlePath1, "HEAD")

	push1 := doRequest(t, handler, http.MethodPost, "/api/git/push", created.APIKey, mustReadFile(t, bundlePath1), "application/octet-stream")
	if push1.Code != http.StatusOK {
		t.Fatalf("initial push status=%d body=%s", push1.Code, push1.Body.String())
	}
	if !gitRefExists(t, repo.Path, gitrepo.CommitRef(rootHash)) {
		t.Fatalf("expected root commit ref after initial push")
	}

	writeFileHTTP(t, filepath.Join(sourceDir, "README.md"), "one\ntwo\n")
	runGitHTTP(t, sourceDir, "add", "README.md")
	runGitHTTP(t, sourceDir, "commit", "-m", "head")
	headHash := strings.TrimSpace(runGitHTTP(t, sourceDir, "rev-parse", "HEAD"))
	bundlePath2 := filepath.Join(t.TempDir(), "push-2.bundle")
	runGitHTTP(t, sourceDir, "bundle", "create", bundlePath2, "HEAD", "^"+rootHash)

	srv := extractServer(t, handler)
	srv.SetCommitIndexerForTests(func(context.Context, []model.Commit, *string) ([]string, error) {
		return nil, errors.New("boom")
	})
	defer srv.SetCommitIndexerForTests(nil)

	push2 := doRequest(t, handler, http.MethodPost, "/api/git/push", created.APIKey, mustReadFile(t, bundlePath2), "application/octet-stream")
	assertJSONError(t, push2, http.StatusInternalServerError, "failed to index imported commits")

	if !gitRefExists(t, repo.Path, gitrepo.CommitRef(rootHash)) {
		t.Fatalf("expected existing ref %s to survive rollback", rootHash)
	}
	if gitRefExists(t, repo.Path, gitrepo.CommitRef(headHash)) {
		t.Fatalf("expected new ref %s to be rolled back", headHash)
	}
}

func newTestServerWithAgent(t *testing.T, opts ...func(*server.Config)) (*gitrepo.Repo, *db.DB, http.Handler, struct {
	ID     string `json:"id"`
	APIKey string `json:"api_key"`
}) {
	t.Helper()

	dataDir := t.TempDir()
	store, err := db.Open(filepath.Join(dataDir, "gitthicket.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	repo, err := gitrepo.Open(filepath.Join(dataDir, "repo.git"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}

	cfg := server.Config{
		MaxBundleBytes:    10 * 1024 * 1024,
		MaxPushesPerHour:  100,
		MaxPostsPerHour:   100,
		MaxCommitsPerPush: 1000,
		DiffMaxBytes:      32 * 1024,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	handler := server.New(cfg, store, repo, auth.NewAuthenticator(store, "admin-secret"))

	createResp := doJSONRequest(t, handler, http.MethodPost, "/api/admin/agents", "admin-secret", map[string]string{"id": "agent-1"})
	if createResp.Code != http.StatusCreated {
		t.Fatalf("create agent status=%d body=%s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	}
	decodeJSONBody(t, createResp.Body.Bytes(), &created)
	return repo, store, handler, created
}

func makeCommitBundle(t *testing.T) (string, string, string, string) {
	t.Helper()
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
	return sourceDir, bundlePath, rootHash, headHash
}

func extractServer(t *testing.T, handler http.Handler) *server.Server {
	t.Helper()
	srv, ok := handler.(*server.Server)
	if !ok {
		t.Fatalf("expected *server.Server, got %T", handler)
	}
	return srv
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", path, err)
	}
	return data
}

func gitRefExists(t *testing.T, repoPath, ref string) bool {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "-C", repoPath, "show-ref", "--verify", "--quiet", ref)
	return cmd.Run() == nil
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
	authorization := ""
	if bearer != "" {
		authorization = "Bearer " + bearer
	}
	return doRawRequest(t, handler, method, path, authorization, body, contentType)
}

func doRawRequest(t *testing.T, handler http.Handler, method, path, authorization string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func assertJSONError(t *testing.T, rr *httptest.ResponseRecorder, status int, contains string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("expected status %d, got %d body=%s", status, rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("expected json content-type, got %q", got)
	}
	var payload struct {
		Error string `json:"error"`
	}
	decodeJSONBody(t, rr.Body.Bytes(), &payload)
	if !strings.Contains(payload.Error, contains) {
		t.Fatalf("expected error containing %q, got %q", contains, payload.Error)
	}
}

func decodeJSONBody(t *testing.T, body []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode json body: %v\n%s", err, string(body))
	}
}
