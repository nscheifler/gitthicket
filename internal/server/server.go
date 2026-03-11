package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gitthicket/internal/auth"
	store "gitthicket/internal/db"
	"gitthicket/internal/gitrepo"
	"gitthicket/internal/model"
	"gitthicket/internal/ratelimit"
)

const (
	defaultListLimit           = 50
	maxListLimit               = 200
	defaultMaxCommitPush       = 10000
	defaultDiffBytes           = 256 * 1024
	jsonBodyLimit        int64 = 64 * 1024
)

type Config struct {
	MaxBundleBytes    int64
	MaxPushesPerHour  int
	MaxPostsPerHour   int
	MaxCommitsPerPush int
	DiffMaxBytes      int
}

type commitIndexer func(context.Context, []model.Commit, *string) ([]string, error)

type Server struct {
	db            *store.DB
	repo          *gitrepo.Repo
	auth          *auth.Authenticator
	limiter       *ratelimit.Limiter
	cfg           Config
	mux           *http.ServeMux
	commitIndexer commitIndexer
}

type errorResponse struct {
	Error string `json:"error"`
}

type pushResponse struct {
	Imported []string `json:"imported"`
	Count    int      `json:"count"`
}

type commitsResponse struct {
	Commits []model.Commit `json:"commits"`
}

type commitResponse struct {
	Commit model.Commit `json:"commit"`
}

type channelsResponse struct {
	Channels []model.Channel `json:"channels"`
}

type channelResponse struct {
	Channel model.Channel `json:"channel"`
}

type postsResponse struct {
	Posts []model.Post `json:"posts"`
}

type postResponse struct {
	Post model.Post `json:"post"`
}

func New(cfg Config, db *store.DB, repo *gitrepo.Repo, authenticator *auth.Authenticator) *Server {
	if cfg.MaxCommitsPerPush <= 0 {
		cfg.MaxCommitsPerPush = defaultMaxCommitPush
	}
	if cfg.DiffMaxBytes <= 0 {
		cfg.DiffMaxBytes = defaultDiffBytes
	}
	s := &Server{
		db:            db,
		repo:          repo,
		auth:          authenticator,
		limiter:       ratelimit.New(),
		cfg:           cfg,
		mux:           http.NewServeMux(),
		commitIndexer: db.UpsertCommits,
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) SetCommitIndexerForTest(fn func(context.Context, []model.Commit, *string) ([]string, error)) {
	if fn == nil {
		s.commitIndexer = s.db.UpsertCommits
		return
	}
	s.commitIndexer = fn
}

func (s *Server) SetCommitIndexerForTests(indexer func(context.Context, []model.Commit, *string) ([]string, error)) {
	if indexer == nil {
		s.commitIndexer = s.db.UpsertCommits
		return
	}
	s.commitIndexer = indexer
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.Handle("POST /api/admin/agents", s.auth.AdminMiddleware(http.HandlerFunc(s.handleCreateAgent)))

	s.mux.Handle("POST /api/git/push", s.auth.AgentMiddleware(http.HandlerFunc(s.handleGitPush)))
	s.mux.Handle("GET /api/git/fetch/{hash}", s.auth.AgentMiddleware(http.HandlerFunc(s.handleGitFetch)))
	s.mux.Handle("GET /api/git/commits", s.auth.AgentMiddleware(http.HandlerFunc(s.handleListCommits)))
	s.mux.Handle("GET /api/git/commits/{hash}", s.auth.AgentMiddleware(http.HandlerFunc(s.handleGetCommit)))
	s.mux.Handle("GET /api/git/commits/{hash}/children", s.auth.AgentMiddleware(http.HandlerFunc(s.handleListChildren)))
	s.mux.Handle("GET /api/git/commits/{hash}/lineage", s.auth.AgentMiddleware(http.HandlerFunc(s.handleLineage)))
	s.mux.Handle("GET /api/git/leaves", s.auth.AgentMiddleware(http.HandlerFunc(s.handleLeaves)))
	s.mux.Handle("GET /api/git/diff/{hashA}/{hashB}", s.auth.AgentMiddleware(http.HandlerFunc(s.handleDiff)))

	s.mux.Handle("GET /api/channels", s.auth.AgentMiddleware(http.HandlerFunc(s.handleListChannels)))
	s.mux.Handle("POST /api/channels", s.auth.AgentMiddleware(http.HandlerFunc(s.handleCreateChannel)))
	s.mux.Handle("GET /api/channels/{name}/posts", s.auth.AgentMiddleware(http.HandlerFunc(s.handleListPosts)))
	s.mux.Handle("POST /api/channels/{name}/posts", s.auth.AgentMiddleware(http.HandlerFunc(s.handleCreatePost)))
	s.mux.Handle("GET /api/posts/{id}", s.auth.AgentMiddleware(http.HandlerFunc(s.handleGetPost)))
	s.mux.Handle("GET /api/posts/{id}/replies", s.auth.AgentMiddleware(http.HandlerFunc(s.handleReplies)))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"time": time.Now().UTC(),
	})
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, jsonBodyLimit, &req) {
		return
	}
	apiKey, err := auth.GenerateAPIKey()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to generate api key"})
		return
	}
	agent, err := s.db.CreateAgent(r.Context(), req.ID, auth.HashAPIKey(apiKey))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      agent.ID,
		"api_key": apiKey,
	})
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.db.ListChannels(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, channelsResponse{Channels: channels})
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, jsonBodyLimit, &req) {
		return
	}
	agent := currentAgent(r.Context())
	channel, err := s.db.CreateChannel(r.Context(), req.Name, &agent.ID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, channelResponse{Channel: *channel})
}

func (s *Server) handleListPosts(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parseLimitOffset(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	posts, err := s.db.ListChannelPosts(r.Context(), r.PathValue("name"), limit, offset)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, postsResponse{Posts: posts})
}

func (s *Server) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	agent := currentAgent(r.Context())
	if !s.allowRate(w, agent.ID, "post", s.cfg.MaxPostsPerHour) {
		return
	}
	var req struct {
		Body         string `json:"body"`
		ParentPostID *int64 `json:"parent_post_id"`
	}
	if !decodeJSON(w, r, jsonBodyLimit, &req) {
		return
	}
	post, err := s.db.CreatePost(r.Context(), r.PathValue("name"), agent.ID, req.Body, req.ParentPostID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, postResponse{Post: *post})
}

func (s *Server) handleGetPost(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid post id"})
		return
	}
	post, err := s.db.GetPost(r.Context(), id)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, postResponse{Post: *post})
}

func (s *Server) handleReplies(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid post id"})
		return
	}
	replies, err := s.db.ListReplies(r.Context(), id)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, postsResponse{Posts: replies})
}

func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request) {
	agent := currentAgent(r.Context())
	if !s.allowRate(w, agent.ID, "push", s.cfg.MaxPushesPerHour) {
		return
	}
	bundlePath, cleanup, err := s.saveBundleUpload(w, r)
	if err != nil {
		return
	}
	defer cleanup()

	staged, err := s.repo.StageBundle(r.Context(), bundlePath)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	defer staged.Cleanup(r.Context())

	commits := staged.Commits()
	hashes := make([]string, 0, len(commits))
	for _, commit := range commits {
		hashes = append(hashes, commit.Hash)
	}
	known, err := s.db.KnownCommitSet(r.Context(), hashes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to check known commits"})
		return
	}
	var newHashes []string
	for _, hash := range hashes {
		if !known[hash] {
			newHashes = append(newHashes, hash)
		}
	}
	if len(newHashes) > s.cfg.MaxCommitsPerPush {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{
			Error: fmt.Sprintf("bundle contains too many new commits (%d > %d)", len(newHashes), s.cfg.MaxCommitsPerPush),
		})
		return
	}
	promoted, err := staged.Promote(r.Context(), newHashes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to promote imported commits"})
		return
	}
	imported, err := s.commitIndexer(r.Context(), commits, &agent.ID)
	if err != nil {
		_ = s.repo.DeleteCommitRefs(context.Background(), promoted.CreatedRefs)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to index imported commits"})
		return
	}
	if imported == nil {
		imported = []string{}
	}
	writeJSON(w, http.StatusOK, pushResponse{
		Imported: imported,
		Count:    len(imported),
	})
}

func (s *Server) handleGitFetch(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(r.PathValue("hash"))
	bundlePath, cleanup, err := s.repo.CreateBundleForCommit(r.Context(), hash)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	defer cleanup()

	file, err := os.Open(bundlePath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to open bundle"})
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to stat bundle"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", hash+".bundle"))
	w.Header().Set("X-GitThicket-Ref", "refs/gitthicket/commits/"+hash)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func (s *Server) handleListCommits(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parseLimitOffset(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	commits, err := s.db.ListCommits(r.Context(), optionalAgentFilter(r.URL.Query().Get("agent")), limit, offset)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commitsResponse{Commits: commits})
}

func (s *Server) handleGetCommit(w http.ResponseWriter, r *http.Request) {
	commit, err := s.db.GetCommit(r.Context(), strings.ToLower(r.PathValue("hash")))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commitResponse{Commit: *commit})
}

func (s *Server) handleListChildren(w http.ResponseWriter, r *http.Request) {
	commits, err := s.db.ListChildren(r.Context(), strings.ToLower(r.PathValue("hash")))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commitsResponse{Commits: commits})
}

func (s *Server) handleLineage(w http.ResponseWriter, r *http.Request) {
	commits, err := s.db.Lineage(r.Context(), strings.ToLower(r.PathValue("hash")))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commitsResponse{Commits: commits})
}

func (s *Server) handleLeaves(w http.ResponseWriter, r *http.Request) {
	commits, err := s.db.ListLeaves(r.Context())
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commitsResponse{Commits: commits})
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	diff, truncated, err := s.repo.Diff(r.Context(), r.PathValue("hashA"), r.PathValue("hashB"), s.cfg.DiffMaxBytes)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if truncated {
		w.Header().Set("X-GitThicket-Diff-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, diff)
}

func currentAgent(ctx context.Context) *model.Agent {
	agent, _ := auth.AgentFromContext(ctx)
	return agent
}

func (s *Server) allowRate(w http.ResponseWriter, agentID, action string, limit int) bool {
	decision := s.limiter.Allow(agentID, action, limit, time.Now().UTC())
	if decision.Allowed {
		return true
	}
	retryAfter := int64(time.Until(decision.ResetAt).Seconds())
	if retryAfter < 0 {
		retryAfter = 0
	}
	w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
	writeJSON(w, http.StatusTooManyRequests, errorResponse{
		Error: fmt.Sprintf("rate limit exceeded for %s; resets at %s", action, decision.ResetAt.Format(time.RFC3339)),
	})
	return false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
		return false
	}
	if decoder.More() {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "request body must contain a single JSON object"})
		return false
	}
	return true
}

func parseLimitOffset(r *http.Request) (int, int, error) {
	limit := defaultListLimit
	offset := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return 0, 0, errors.New("limit must be a positive integer")
		}
		if value > maxListLimit {
			value = maxListLimit
		}
		limit = value
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return 0, 0, errors.New("offset must be a non-negative integer")
		}
		offset = value
	}
	return limit, offset, nil
}

func parseIDParam(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound) || errors.Is(err, gitrepo.ErrNotFound)
}

func isConflict(err error) bool {
	return errors.Is(err, store.ErrConflict)
}

func isInvalid(err error) bool {
	return errors.Is(err, store.ErrInvalidInput) || errors.Is(err, gitrepo.ErrInvalidBundle)
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case isNotFound(err):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
	case isConflict(err):
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
	case isInvalid(err):
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
	}
}

func optionalAgentFilter(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func (s *Server) saveBundleUpload(w http.ResponseWriter, r *http.Request) (string, func(), error) {
	limit := s.cfg.MaxBundleBytes + (1 << 20)
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	file, err := os.CreateTemp("", "gitthicket-upload-*.bundle")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to create temp bundle"})
		return "", nil, err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}

	contentType := r.Header.Get("Content-Type")
	var src io.ReadCloser
	switch {
	case strings.HasPrefix(contentType, "multipart/form-data"):
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			cleanup()
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "bundle upload too large"})
				return "", nil, err
			}
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid multipart upload"})
			return "", nil, err
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		src, _, err = r.FormFile("bundle")
		if err != nil {
			cleanup()
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing bundle file part"})
			return "", nil, err
		}
	case contentType == "" || strings.HasPrefix(contentType, "application/octet-stream"):
		src = r.Body
	default:
		cleanup()
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "unsupported content type"})
		return "", nil, errors.New("unsupported content type")
	}
	defer src.Close()

	written, err := io.Copy(file, io.LimitReader(src, s.cfg.MaxBundleBytes+1))
	if err != nil {
		cleanup()
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "bundle upload too large"})
			return "", nil, err
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "failed to read uploaded bundle"})
		return "", nil, err
	}
	if written == 0 {
		cleanup()
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "bundle upload is empty"})
		return "", nil, errors.New("empty bundle")
	}
	if written > s.cfg.MaxBundleBytes {
		cleanup()
		writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "bundle upload too large"})
		return "", nil, errors.New("bundle too large")
	}
	if err := file.Close(); err != nil {
		cleanup()
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to finalize uploaded bundle"})
		return "", nil, err
	}
	return file.Name(), cleanup, nil
}
