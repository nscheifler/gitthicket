package db_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gitthicket/internal/db"
	"gitthicket/internal/model"
)

func TestBootstrapPostsAndLineage(t *testing.T) {
	t.Parallel()

	store, err := db.Open(filepath.Join(t.TempDir(), "gitthicket.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	channels, err := store.ListChannels(ctx)
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	if len(channels) != 1 || channels[0].Name != "general" {
		t.Fatalf("expected bootstrapped general channel, got %#v", channels)
	}

	agent, err := store.CreateAgent(ctx, "agent-1", "hash-1")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	channel, err := store.CreateChannel(ctx, "Team Notes", &agent.ID)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if channel.Name != "team-notes" {
		t.Fatalf("expected normalized channel name, got %q", channel.Name)
	}

	post, err := store.CreatePost(ctx, channel.Name, agent.ID, "top level", nil)
	if err != nil {
		t.Fatalf("create post: %v", err)
	}
	reply, err := store.CreatePost(ctx, channel.Name, agent.ID, "reply", &post.ID)
	if err != nil {
		t.Fatalf("create reply: %v", err)
	}
	if reply.ParentPostID == nil || *reply.ParentPostID != post.ID {
		t.Fatalf("expected reply parent %d, got %#v", post.ID, reply.ParentPostID)
	}

	posts, err := store.ListChannelPosts(ctx, channel.Name, 10, 0)
	if err != nil {
		t.Fatalf("list posts: %v", err)
	}
	if len(posts) != 1 || posts[0].ID != post.ID {
		t.Fatalf("expected only top-level post, got %#v", posts)
	}

	replies, err := store.ListReplies(ctx, post.ID)
	if err != nil {
		t.Fatalf("list replies: %v", err)
	}
	if len(replies) != 1 || replies[0].ID != reply.ID {
		t.Fatalf("expected single reply, got %#v", replies)
	}

	agentID := agent.ID
	rootTime := time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	headTime := rootTime.Add(2 * time.Minute)
	commits := []model.Commit{
		{
			Hash:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			AgentID:        &agentID,
			AuthorName:     "Agent One",
			AuthorEmail:    "agent@example.com",
			CommitterName:  "Agent One",
			CommitterEmail: "agent@example.com",
			Subject:        "root",
			Body:           "root body",
			CreatedAt:      rootTime,
			CommitTime:     rootTime,
		},
		{
			Hash:           "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			AgentID:        &agentID,
			AuthorName:     "Agent One",
			AuthorEmail:    "agent@example.com",
			CommitterName:  "Agent One",
			CommitterEmail: "agent@example.com",
			Subject:        "head",
			Body:           "head body",
			CreatedAt:      headTime,
			CommitTime:     headTime,
			ParentHashes:   []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
	}
	imported, err := store.UpsertCommits(ctx, commits, &agentID)
	if err != nil {
		t.Fatalf("upsert commits: %v", err)
	}
	if len(imported) != 2 {
		t.Fatalf("expected 2 imported commits, got %d", len(imported))
	}

	leaves, err := store.ListLeaves(ctx)
	if err != nil {
		t.Fatalf("list leaves: %v", err)
	}
	if len(leaves) != 1 || leaves[0].Hash != commits[1].Hash {
		t.Fatalf("expected head leaf, got %#v", leaves)
	}

	lineage, err := store.Lineage(ctx, commits[1].Hash)
	if err != nil {
		t.Fatalf("lineage: %v", err)
	}
	if len(lineage) != 2 || lineage[0].Hash != commits[1].Hash || lineage[1].Hash != commits[0].Hash {
		t.Fatalf("unexpected lineage: %#v", lineage)
	}
}
