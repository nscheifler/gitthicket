package gitrepo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gitthicket/internal/model"
)

const (
	commitRefPrefix       = "refs/gitthicket/commits/"
	incomingRefPrefix     = "refs/gitthicket/incoming/"
	maxBundleHeads        = 64
	defaultCommandTimeout = 2 * time.Minute
)

var (
	ErrNotFound      = errors.New("not found")
	ErrInvalidBundle = errors.New("invalid bundle")
	hashPattern      = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

type Repo struct {
	Path string
}

type StagedBundle struct {
	repo     *Repo
	tempRefs []string
	commits  []model.Commit
}

type bundleHead struct {
	Hash   string
	Source string
}

func Open(path string) (*Repo, error) {
	repo := &Repo{Path: path}
	if err := repo.init(context.Background()); err != nil {
		return nil, err
	}
	return repo, nil
}

func (r *Repo) init(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
		return fmt.Errorf("create repo dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(r.Path, "HEAD")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat repo: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCommandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", "init", "--bare", r.Path)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("init bare repo: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if len(out) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(r.Path, "HEAD")); err != nil {
		return fmt.Errorf("init bare repo: %w", err)
	}
	return nil
}

func CommitRef(hash string) string {
	return commitRefPrefix + strings.ToLower(hash)
}

func (r *Repo) CleanupTransientRefs(ctx context.Context) error {
	out, err := r.runGit(ctx, "for-each-ref", "--format=%(refname)", incomingRefPrefix)
	if err != nil {
		return fmt.Errorf("list transient refs: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var refs []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		refs = append(refs, line)
	}
	return r.deleteRefs(ctx, refs)
}

func (r *Repo) ResolveCommit(ctx context.Context, rev string) (string, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return "", ErrNotFound
	}
	out, err := r.runGit(ctx, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("resolve commit %s: %w", rev, err)
	}
	return strings.ToLower(strings.TrimSpace(string(out))), nil
}

func (r *Repo) StageBundle(ctx context.Context, bundlePath string) (*StagedBundle, error) {
	heads, prerequisites, err := r.inspectBundle(ctx, bundlePath)
	if err != nil {
		return nil, err
	}
	if len(heads) == 0 {
		return nil, fmt.Errorf("%w: bundle has no heads", ErrInvalidBundle)
	}
	if len(heads) > maxBundleHeads {
		return nil, fmt.Errorf("%w: bundle contains too many heads", ErrInvalidBundle)
	}

	stageID, err := randomID(8)
	if err != nil {
		return nil, fmt.Errorf("generate stage id: %w", err)
	}
	tempRefs := make([]string, 0, len(heads))
	specs := make([]string, 0, len(heads))
	for i, head := range heads {
		tempRef := fmt.Sprintf("%s%s/%d", incomingRefPrefix, stageID, i)
		tempRefs = append(tempRefs, tempRef)
		specs = append(specs, head.Source+":"+tempRef)
	}

	args := append([]string{"fetch", "--quiet", bundlePath}, specs...)
	if _, err := r.runGit(ctx, args...); err != nil {
		r.deleteRefs(context.Background(), tempRefs)
		return nil, fmt.Errorf("%w: fetch bundle into staging refs: %v", ErrInvalidBundle, err)
	}

	commits, err := r.enumerateCommits(ctx, tempRefs, prerequisites)
	if err != nil {
		r.deleteRefs(context.Background(), tempRefs)
		return nil, err
	}

	return &StagedBundle{
		repo:     r,
		tempRefs: tempRefs,
		commits:  commits,
	}, nil
}

func (s *StagedBundle) Commits() []model.Commit {
	commits := make([]model.Commit, len(s.commits))
	copy(commits, s.commits)
	return commits
}

func (s *StagedBundle) Promote(ctx context.Context, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	for _, hash := range hashes {
		if !validHash(hash) {
			return fmt.Errorf("invalid commit hash %q", hash)
		}
		if _, err := s.repo.runGit(ctx, "update-ref", CommitRef(hash), hash); err != nil {
			return fmt.Errorf("promote commit ref %s: %w", hash, err)
		}
	}
	return nil
}

func (s *StagedBundle) Cleanup(ctx context.Context) error {
	return s.repo.deleteRefs(ctx, s.tempRefs)
}

func (r *Repo) HasCommit(ctx context.Context, hash string) (bool, error) {
	if !validHash(hash) {
		return false, nil
	}
	_, err := r.runGit(ctx, "rev-parse", "--verify", hash+"^{commit}")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, err
}

func (r *Repo) CreateBundleForCommit(ctx context.Context, hash string) (string, func(), error) {
	ok, err := r.HasCommit(ctx, hash)
	if err != nil {
		return "", nil, fmt.Errorf("check commit: %w", err)
	}
	if !ok {
		return "", nil, ErrNotFound
	}
	ref := CommitRef(hash)
	if _, err := r.runGit(ctx, "update-ref", ref, hash); err != nil {
		return "", nil, fmt.Errorf("ensure commit ref: %w", err)
	}

	file, err := os.CreateTemp("", "gitthicket-fetch-*.bundle")
	if err != nil {
		return "", nil, fmt.Errorf("create temp bundle: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", nil, fmt.Errorf("close temp bundle: %w", err)
	}
	if _, err := r.runGit(ctx, "bundle", "create", path, ref); err != nil {
		os.Remove(path)
		return "", nil, fmt.Errorf("create bundle for %s: %w", hash, err)
	}
	cleanup := func() {
		_ = os.Remove(path)
	}
	return path, cleanup, nil
}

func (r *Repo) Diff(ctx context.Context, hashA, hashB string, maxBytes int) (string, bool, error) {
	if !validHash(hashA) || !validHash(hashB) {
		return "", false, ErrNotFound
	}
	out, err := r.runGit(ctx, "diff", "--no-color", hashA, hashB)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", false, ErrNotFound
		}
		return "", false, fmt.Errorf("git diff: %w", err)
	}
	truncated := false
	if maxBytes > 0 && len(out) > maxBytes {
		truncated = true
		marker := "\n\n[gitthicket diff truncated]\n"
		out = append(out[:maxBytes], marker...)
	}
	return string(out), truncated, nil
}

func (r *Repo) inspectBundle(ctx context.Context, bundlePath string) ([]bundleHead, []string, error) {
	if _, err := r.runGit(ctx, "bundle", "verify", bundlePath); err != nil {
		return nil, nil, fmt.Errorf("%w: git bundle verify failed: %v", ErrInvalidBundle, err)
	}
	out, err := r.runGit(ctx, "bundle", "list-heads", bundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: list bundle heads: %v", ErrInvalidBundle, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	heads := make([]bundleHead, 0, len(lines))
	var prerequisites []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		hash := fields[0]
		if strings.HasPrefix(hash, "-") {
			hash = strings.TrimPrefix(hash, "-")
			if validHash(hash) {
				prerequisites = append(prerequisites, strings.ToLower(hash))
			}
			continue
		}
		if !validHash(hash) {
			return nil, nil, fmt.Errorf("%w: invalid head hash %q", ErrInvalidBundle, hash)
		}
		source := strings.ToLower(hash)
		if len(fields) > 1 {
			source = fields[1]
			if !r.validHeadSource(ctx, source) {
				return nil, nil, fmt.Errorf("%w: invalid head ref %q", ErrInvalidBundle, source)
			}
		}
		heads = append(heads, bundleHead{
			Hash:   strings.ToLower(hash),
			Source: source,
		})
	}
	return heads, prerequisites, nil
}

func (r *Repo) validHeadSource(ctx context.Context, source string) bool {
	if source == "HEAD" {
		return true
	}
	if validHash(source) {
		return true
	}
	if !strings.HasPrefix(source, "refs/") {
		return false
	}
	_, err := r.runGit(ctx, "check-ref-format", source)
	return err == nil
}

func (r *Repo) enumerateCommits(ctx context.Context, tempRefs, prerequisites []string) ([]model.Commit, error) {
	parents, err := r.readParentMap(ctx, tempRefs, prerequisites)
	if err != nil {
		return nil, err
	}
	if len(parents) == 0 {
		return nil, nil
	}
	revisionArgs := revArgs(tempRefs, prerequisites)
	format := "%H%x1f%an%x1f%ae%x1f%cn%x1f%ce%x1f%s%x1f%b%x1f%cI%x1e"
	args := append([]string{"log", "--topo-order", "--format=" + format}, revisionArgs...)
	out, err := r.runGit(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: read commit metadata: %v", ErrInvalidBundle, err)
	}
	records := bytes.Split(out, []byte{0x1e})
	filtered := make([][]byte, 0, len(records))
	for _, record := range records {
		record = bytes.TrimSpace(record)
		if len(record) == 0 {
			continue
		}
		filtered = append(filtered, record)
	}
	if len(filtered) == 0 {
		return nil, nil
	}

	base := time.Now().UTC().Add(time.Duration(len(filtered)) * time.Microsecond)
	commits := make([]model.Commit, 0, len(filtered))
	for i, record := range filtered {
		fields := bytes.Split(record, []byte{0x1f})
		if len(fields) != 8 {
			return nil, fmt.Errorf("%w: unexpected git log record format", ErrInvalidBundle)
		}
		hash := strings.ToLower(string(fields[0]))
		commitTime, err := time.Parse(time.RFC3339, string(fields[7]))
		if err != nil {
			return nil, fmt.Errorf("%w: parse commit time for %s: %v", ErrInvalidBundle, hash, err)
		}
		commit := model.Commit{
			Hash:           hash,
			AuthorName:     string(fields[1]),
			AuthorEmail:    string(fields[2]),
			CommitterName:  string(fields[3]),
			CommitterEmail: string(fields[4]),
			Subject:        string(fields[5]),
			Body:           strings.TrimRight(string(fields[6]), "\n"),
			CreatedAt:      base.Add(-time.Duration(i) * time.Microsecond),
			CommitTime:     commitTime.UTC(),
			ParentHashes:   parents[hash],
		}
		commits = append(commits, commit)
	}
	sort.SliceStable(commits, func(i, j int) bool {
		return commits[i].CreatedAt.After(commits[j].CreatedAt)
	})
	return commits, nil
}

func (r *Repo) readParentMap(ctx context.Context, tempRefs, prerequisites []string) (map[string][]string, error) {
	revisionArgs := revArgs(tempRefs, prerequisites)
	args := append([]string{"rev-list", "--parents", "--topo-order"}, revisionArgs...)
	out, err := r.runGit(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: enumerate commits: %v", ErrInvalidBundle, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	parents := make(map[string][]string, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		hash := strings.ToLower(fields[0])
		for _, parent := range fields[1:] {
			parents[hash] = append(parents[hash], strings.ToLower(parent))
		}
		if _, ok := parents[hash]; !ok {
			parents[hash] = nil
		}
	}
	return parents, nil
}

func (r *Repo) deleteRefs(ctx context.Context, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	var firstErr error
	for _, ref := range refs {
		if _, err := r.runGit(ctx, "update-ref", "-d", ref); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("delete ref %s: %w", ref, err)
		}
	}
	return firstErr
}

func (r *Repo) runGit(ctx context.Context, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCommandTimeout)
		defer cancel()
	}
	cmdArgs := make([]string, 0, len(args)+2)
	if filepath.IsAbs(r.Path) {
		cmdArgs = append(cmdArgs, "-C", r.Path)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func revArgs(tempRefs, prerequisites []string) []string {
	args := make([]string, 0, len(tempRefs)+len(prerequisites))
	args = append(args, tempRefs...)
	for _, prerequisite := range prerequisites {
		args = append(args, "^"+prerequisite)
	}
	return args
}

func validHash(hash string) bool {
	return hashPattern.MatchString(hash)
}

func randomID(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
