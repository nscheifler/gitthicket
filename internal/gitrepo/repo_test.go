package gitrepo_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitthicket/internal/gitrepo"
)

func TestStagePromoteExportAndDiff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sourceDir := t.TempDir()
	runGit(t, sourceDir, "init")
	runGit(t, sourceDir, "config", "user.name", "Agent One")
	runGit(t, sourceDir, "config", "user.email", "agent@example.com")

	writeFile(t, filepath.Join(sourceDir, "README.md"), "hello\n")
	runGit(t, sourceDir, "add", "README.md")
	runGit(t, sourceDir, "commit", "-m", "root")
	rootHash := strings.TrimSpace(runGit(t, sourceDir, "rev-parse", "HEAD"))

	writeFile(t, filepath.Join(sourceDir, "README.md"), "hello\nworld\n")
	runGit(t, sourceDir, "add", "README.md")
	runGit(t, sourceDir, "commit", "-m", "second")
	headHash := strings.TrimSpace(runGit(t, sourceDir, "rev-parse", "HEAD"))

	bundlePath := filepath.Join(t.TempDir(), "push.bundle")
	runGit(t, sourceDir, "bundle", "create", bundlePath, "HEAD")

	repo, err := gitrepo.Open(filepath.Join(t.TempDir(), "repo.git"))
	if err != nil {
		t.Fatalf("open bare repo: %v", err)
	}
	stage, err := repo.StageBundle(ctx, bundlePath)
	if err != nil {
		t.Fatalf("stage bundle: %v", err)
	}
	defer stage.Cleanup(ctx)

	commits := stage.Commits()
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}
	hashes := []string{commits[0].Hash, commits[1].Hash}
	if _, err := stage.Promote(ctx, hashes); err != nil {
		t.Fatalf("promote: %v", err)
	}

	ok, err := repo.HasCommit(ctx, headHash)
	if err != nil {
		t.Fatalf("has commit: %v", err)
	}
	if !ok {
		t.Fatalf("expected promoted head %s to exist", headHash)
	}

	diff, truncated, err := repo.Diff(ctx, rootHash, headHash, 4096)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if truncated {
		t.Fatalf("did not expect diff truncation")
	}
	if !strings.Contains(diff, "+world") {
		t.Fatalf("expected diff to contain new line, got %q", diff)
	}

	exportPath, cleanup, err := repo.CreateBundleForCommit(ctx, headHash)
	if err != nil {
		t.Fatalf("export bundle: %v", err)
	}
	defer cleanup()

	if _, err := os.Stat(exportPath); err != nil {
		t.Fatalf("stat export bundle: %v", err)
	}

	listHeads := runGit(t, t.TempDir(), "-C", repo.Path, "bundle", "list-heads", exportPath)
	if !strings.Contains(listHeads, gitrepo.CommitRef(headHash)) {
		t.Fatalf("expected exported bundle ref, got %q", listHeads)
	}
}

func TestRejectedBundleDoesNotDirtyCanonicalRepo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, err := gitrepo.Open(filepath.Join(t.TempDir(), "repo.git"))
	if err != nil {
		t.Fatalf("open bare repo: %v", err)
	}
	beforeObjects := objectInventory(t, repo.Path)
	beforeRefs := runGit(t, t.TempDir(), "-C", repo.Path, "for-each-ref", "--format=%(refname)")

	sourceDir := t.TempDir()
	runGit(t, sourceDir, "init")
	runGit(t, sourceDir, "config", "user.name", "Agent One")
	runGit(t, sourceDir, "config", "user.email", "agent@example.com")
	writeFile(t, filepath.Join(sourceDir, "blob.txt"), "hello\n")
	runGit(t, sourceDir, "add", "blob.txt")
	runGit(t, sourceDir, "commit", "-m", "root")
	blobHash := strings.TrimSpace(runGit(t, sourceDir, "rev-parse", "HEAD:blob.txt"))
	runGit(t, sourceDir, "update-ref", "refs/test/blob", blobHash)

	bundlePath := filepath.Join(t.TempDir(), "blob.bundle")
	runGit(t, sourceDir, "bundle", "create", bundlePath, "refs/test/blob")

	_, err = repo.StageBundle(ctx, bundlePath)
	if err == nil || !strings.Contains(err.Error(), "advertised head does not resolve to a commit") {
		t.Fatalf("expected non-commit rejection, got %v", err)
	}

	afterObjects := objectInventory(t, repo.Path)
	afterRefs := runGit(t, t.TempDir(), "-C", repo.Path, "for-each-ref", "--format=%(refname)")
	if afterObjects != beforeObjects {
		t.Fatalf("expected object inventory to stay unchanged, before=%q after=%q", beforeObjects, afterObjects)
	}
	if afterRefs != beforeRefs {
		t.Fatalf("expected refs to stay unchanged, before=%q after=%q", beforeRefs, afterRefs)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func objectInventory(t *testing.T, repoPath string) string {
	t.Helper()
	return runGit(t, t.TempDir(), "-C", repoPath, "count-objects", "-v")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}
