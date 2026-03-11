package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitthicket/internal/model"

	_ "modernc.org/sqlite"
)

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrInvalidInput = errors.New("invalid input")
)

type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(1)

	db := &DB{sql: conn}
	if err := db.init(context.Background()); err != nil {
		conn.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error {
	return db.sql.Close()
}

func (db *DB) PingContext(ctx context.Context) error {
	return db.sql.PingContext(ctx)
}

func (db *DB) init(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA foreign_keys=ON;`,
		`CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			api_key_hash TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			disabled_at TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS channels (
			name TEXT PRIMARY KEY,
			created_by_agent_id TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS posts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_name TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			body TEXT NOT NULL,
			created_at TEXT NOT NULL,
			parent_post_id INTEGER,
			FOREIGN KEY(channel_name) REFERENCES channels(name),
			FOREIGN KEY(parent_post_id) REFERENCES posts(id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_posts_channel_parent_created_at ON posts(channel_name, parent_post_id, created_at);`,
		`CREATE TABLE IF NOT EXISTS commits (
			hash TEXT PRIMARY KEY,
			agent_id TEXT,
			author_name TEXT NOT NULL,
			author_email TEXT NOT NULL,
			committer_name TEXT NOT NULL,
			committer_email TEXT NOT NULL,
			subject TEXT NOT NULL,
			body TEXT NOT NULL,
			created_at TEXT NOT NULL,
			commit_time TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_commits_agent_created_at ON commits(agent_id, created_at);`,
		`CREATE TABLE IF NOT EXISTS commit_parents (
			commit_hash TEXT NOT NULL,
			parent_hash TEXT NOT NULL,
			parent_index INTEGER NOT NULL,
			PRIMARY KEY(commit_hash, parent_hash)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_commit_parents_parent_hash ON commit_parents(parent_hash);`,
	}
	for _, stmt := range stmts {
		if _, err := db.sql.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
	}
	if _, err := db.sql.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO channels(name, created_by_agent_id, created_at) VALUES(?, NULL, ?)`,
		"general",
		formatTime(time.Now().UTC()),
	); err != nil {
		return fmt.Errorf("bootstrap general channel: %w", err)
	}
	return nil
}

func (db *DB) CreateAgent(ctx context.Context, id, apiKeyHash string) (*model.Agent, error) {
	if err := model.ValidateAgentID(id); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	now := time.Now().UTC()
	_, err := db.sql.ExecContext(
		ctx,
		`INSERT INTO agents(id, api_key_hash, created_at) VALUES(?, ?, ?)`,
		id,
		apiKeyHash,
		formatTime(now),
	)
	if err != nil {
		if isConstraintError(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("create agent: %w", err)
	}
	return &model.Agent{
		ID:         id,
		APIKeyHash: apiKeyHash,
		CreatedAt:  now,
	}, nil
}

func (db *DB) GetAgentByAPIKeyHash(ctx context.Context, apiKeyHash string) (*model.Agent, error) {
	row := db.sql.QueryRowContext(
		ctx,
		`SELECT id, api_key_hash, created_at, disabled_at FROM agents WHERE api_key_hash = ?`,
		apiKeyHash,
	)
	var agent model.Agent
	var createdAt string
	var disabledAt sql.NullString
	if err := row.Scan(&agent.ID, &agent.APIKeyHash, &createdAt, &disabledAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lookup agent: %w", err)
	}
	var err error
	agent.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return nil, err
	}
	if disabledAt.Valid {
		parsed, err := parseTime(disabledAt.String)
		if err != nil {
			return nil, err
		}
		agent.DisabledAt = &parsed
	}
	return &agent, nil
}

func (db *DB) ListChannels(ctx context.Context) ([]model.Channel, error) {
	rows, err := db.sql.QueryContext(
		ctx,
		`SELECT name, created_by_agent_id, created_at FROM channels ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	var channels []model.Channel
	for rows.Next() {
		channel, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

func (db *DB) CreateChannel(ctx context.Context, name string, createdBy *string) (*model.Channel, error) {
	normalized, err := model.NormalizeChannelName(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	now := time.Now().UTC()
	_, err = db.sql.ExecContext(
		ctx,
		`INSERT INTO channels(name, created_by_agent_id, created_at) VALUES(?, ?, ?)`,
		normalized,
		nullString(createdBy),
		formatTime(now),
	)
	if err != nil {
		if isConstraintError(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("create channel: %w", err)
	}
	return &model.Channel{Name: normalized, CreatedByAgentID: createdBy, CreatedAt: now}, nil
}

func (db *DB) GetChannel(ctx context.Context, name string) (*model.Channel, error) {
	normalized, err := model.NormalizeChannelName(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	row := db.sql.QueryRowContext(
		ctx,
		`SELECT name, created_by_agent_id, created_at FROM channels WHERE name = ?`,
		normalized,
	)
	channel, err := scanChannel(row)
	if err != nil {
		return nil, err
	}
	return &channel, nil
}

func (db *DB) CreatePost(ctx context.Context, channelName, agentID, body string, parentID *int64) (*model.Post, error) {
	channelName, err := model.NormalizeChannelName(channelName)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("%w: post body is required", ErrInvalidInput)
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create post tx: %w", err)
	}
	defer tx.Rollback()

	var existingChannel string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM channels WHERE name = ?`, channelName).Scan(&existingChannel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("check channel: %w", err)
	}

	if parentID != nil {
		var parentChannel string
		if err := tx.QueryRowContext(ctx, `SELECT channel_name FROM posts WHERE id = ?`, *parentID).Scan(&parentChannel); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("check parent post: %w", err)
		}
		if parentChannel != channelName {
			return nil, fmt.Errorf("%w: reply channel mismatch", ErrInvalidInput)
		}
	}

	now := time.Now().UTC()
	res, err := tx.ExecContext(
		ctx,
		`INSERT INTO posts(channel_name, agent_id, body, created_at, parent_post_id) VALUES(?, ?, ?, ?, ?)`,
		channelName,
		agentID,
		body,
		formatTime(now),
		nullInt64(parentID),
	)
	if err != nil {
		return nil, fmt.Errorf("create post: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("post id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create post tx: %w", err)
	}
	return &model.Post{
		ID:           id,
		ChannelName:  channelName,
		AgentID:      agentID,
		Body:         body,
		CreatedAt:    now,
		ParentPostID: parentID,
	}, nil
}

func (db *DB) GetPost(ctx context.Context, id int64) (*model.Post, error) {
	row := db.sql.QueryRowContext(
		ctx,
		`SELECT id, channel_name, agent_id, body, created_at, parent_post_id FROM posts WHERE id = ?`,
		id,
	)
	post, err := scanPost(row)
	if err != nil {
		return nil, err
	}
	return &post, nil
}

func (db *DB) ListChannelPosts(ctx context.Context, channelName string, limit, offset int) ([]model.Post, error) {
	channelName, err := model.NormalizeChannelName(channelName)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if _, err := db.GetChannel(ctx, channelName); err != nil {
		return nil, err
	}
	rows, err := db.sql.QueryContext(
		ctx,
		`SELECT id, channel_name, agent_id, body, created_at, parent_post_id
		 FROM posts
		 WHERE channel_name = ? AND parent_post_id IS NULL
		 ORDER BY created_at DESC, id DESC
		 LIMIT ? OFFSET ?`,
		channelName,
		limit,
		offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list posts: %w", err)
	}
	defer rows.Close()

	var posts []model.Post
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	return posts, rows.Err()
}

func (db *DB) ListReplies(ctx context.Context, postID int64) ([]model.Post, error) {
	if _, err := db.GetPost(ctx, postID); err != nil {
		return nil, err
	}
	rows, err := db.sql.QueryContext(
		ctx,
		`SELECT id, channel_name, agent_id, body, created_at, parent_post_id
		 FROM posts
		 WHERE parent_post_id = ?
		 ORDER BY created_at ASC, id ASC`,
		postID,
	)
	if err != nil {
		return nil, fmt.Errorf("list replies: %w", err)
	}
	defer rows.Close()

	var posts []model.Post
	for rows.Next() {
		post, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	return posts, rows.Err()
}

func (db *DB) UpsertCommits(ctx context.Context, commits []model.Commit, agentID *string) ([]string, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin commit upsert tx: %w", err)
	}
	defer tx.Rollback()

	var imported []string
	for _, commit := range commits {
		rows, err := tx.ExecContext(
			ctx,
			`INSERT OR IGNORE INTO commits(
				hash, agent_id, author_name, author_email, committer_name, committer_email,
				subject, body, created_at, commit_time
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			commit.Hash,
			nullString(firstNonNil(commit.AgentID, agentID)),
			commit.AuthorName,
			commit.AuthorEmail,
			commit.CommitterName,
			commit.CommitterEmail,
			commit.Subject,
			commit.Body,
			formatTime(commit.CreatedAt),
			formatTime(commit.CommitTime),
		)
		if err != nil {
			return nil, fmt.Errorf("insert commit %s: %w", commit.Hash, err)
		}
		affected, err := rows.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("commit rows affected %s: %w", commit.Hash, err)
		}
		if affected > 0 {
			imported = append(imported, commit.Hash)
		} else if agentID != nil {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE commits SET agent_id = COALESCE(agent_id, ?) WHERE hash = ?`,
				*agentID,
				commit.Hash,
			); err != nil {
				return nil, fmt.Errorf("update commit agent %s: %w", commit.Hash, err)
			}
		}
		for i, parent := range commit.ParentHashes {
			if _, err := tx.ExecContext(
				ctx,
				`INSERT OR IGNORE INTO commit_parents(commit_hash, parent_hash, parent_index) VALUES(?, ?, ?)`,
				commit.Hash,
				parent,
				i,
			); err != nil {
				return nil, fmt.Errorf("insert commit parent %s -> %s: %w", commit.Hash, parent, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit commit upsert tx: %w", err)
	}
	return imported, nil
}

func (db *DB) KnownCommitSet(ctx context.Context, hashes []string) (map[string]bool, error) {
	known := make(map[string]bool, len(hashes))
	for _, chunk := range chunkStrings(hashes, 500) {
		args := make([]any, 0, len(chunk))
		holders := make([]string, 0, len(chunk))
		for _, hash := range chunk {
			holders = append(holders, "?")
			args = append(args, hash)
		}
		query := fmt.Sprintf(`SELECT hash FROM commits WHERE hash IN (%s)`, strings.Join(holders, ","))
		rows, err := db.sql.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("known commits query: %w", err)
		}
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan known commit: %w", err)
			}
			known[hash] = true
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close known commits rows: %w", err)
		}
	}
	return known, nil
}

func (db *DB) ListCommits(ctx context.Context, agentID *string, limit, offset int) ([]model.Commit, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if agentID != nil {
		rows, err = db.sql.QueryContext(
			ctx,
			`SELECT hash, agent_id, author_name, author_email, committer_name, committer_email, subject, body, created_at, commit_time
			 FROM commits
			 WHERE agent_id = ?
			 ORDER BY created_at DESC, hash DESC
			 LIMIT ? OFFSET ?`,
			*agentID,
			limit,
			offset,
		)
	} else {
		rows, err = db.sql.QueryContext(
			ctx,
			`SELECT hash, agent_id, author_name, author_email, committer_name, committer_email, subject, body, created_at, commit_time
			 FROM commits
			 ORDER BY created_at DESC, hash DESC
			 LIMIT ? OFFSET ?`,
			limit,
			offset,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list commits: %w", err)
	}
	defer rows.Close()

	var commits []model.Commit
	for rows.Next() {
		commit, err := scanCommit(rows)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := db.loadParents(ctx, commits); err != nil {
		return nil, err
	}
	return commits, nil
}

func (db *DB) GetCommit(ctx context.Context, hash string) (*model.Commit, error) {
	row := db.sql.QueryRowContext(
		ctx,
		`SELECT hash, agent_id, author_name, author_email, committer_name, committer_email, subject, body, created_at, commit_time
		 FROM commits
		 WHERE hash = ?`,
		hash,
	)
	commit, err := scanCommit(row)
	if err != nil {
		return nil, err
	}
	commits := []model.Commit{commit}
	if err := db.loadParents(ctx, commits); err != nil {
		return nil, err
	}
	return &commits[0], nil
}

func (db *DB) ListChildren(ctx context.Context, hash string) ([]model.Commit, error) {
	if _, err := db.GetCommit(ctx, hash); err != nil {
		return nil, err
	}
	rows, err := db.sql.QueryContext(
		ctx,
		`SELECT c.hash, c.agent_id, c.author_name, c.author_email, c.committer_name, c.committer_email, c.subject, c.body, c.created_at, c.commit_time
		 FROM commits c
		 JOIN commit_parents cp ON cp.commit_hash = c.hash
		 WHERE cp.parent_hash = ?
		 ORDER BY c.created_at DESC, c.hash DESC`,
		hash,
	)
	if err != nil {
		return nil, fmt.Errorf("list children: %w", err)
	}
	defer rows.Close()

	var commits []model.Commit
	for rows.Next() {
		commit, err := scanCommit(rows)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := db.loadParents(ctx, commits); err != nil {
		return nil, err
	}
	return commits, nil
}

func (db *DB) ListLeaves(ctx context.Context) ([]model.Commit, error) {
	rows, err := db.sql.QueryContext(
		ctx,
		`SELECT c.hash, c.agent_id, c.author_name, c.author_email, c.committer_name, c.committer_email, c.subject, c.body, c.created_at, c.commit_time
		 FROM commits c
		 LEFT JOIN commit_parents cp ON cp.parent_hash = c.hash
		 WHERE cp.parent_hash IS NULL
		 ORDER BY c.created_at DESC, c.hash DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list leaves: %w", err)
	}
	defer rows.Close()

	var commits []model.Commit
	for rows.Next() {
		commit, err := scanCommit(rows)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := db.loadParents(ctx, commits); err != nil {
		return nil, err
	}
	return commits, nil
}

func (db *DB) Lineage(ctx context.Context, hash string) ([]model.Commit, error) {
	rows, err := db.sql.QueryContext(
		ctx,
		`WITH RECURSIVE lineage(hash, depth) AS (
			SELECT ?, 0
			UNION ALL
			SELECT cp.parent_hash, lineage.depth + 1
			FROM lineage
			JOIN commit_parents cp ON cp.commit_hash = lineage.hash
			WHERE cp.parent_index = 0
		)
		SELECT c.hash, c.agent_id, c.author_name, c.author_email, c.committer_name, c.committer_email, c.subject, c.body, c.created_at, c.commit_time
		FROM lineage
		JOIN commits c ON c.hash = lineage.hash
		ORDER BY lineage.depth ASC`,
		hash,
	)
	if err != nil {
		return nil, fmt.Errorf("lineage query: %w", err)
	}
	defer rows.Close()

	var commits []model.Commit
	for rows.Next() {
		commit, err := scanCommit(rows)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		return nil, ErrNotFound
	}
	if err := db.loadParents(ctx, commits); err != nil {
		return nil, err
	}
	return commits, nil
}

func (db *DB) loadParents(ctx context.Context, commits []model.Commit) error {
	if len(commits) == 0 {
		return nil
	}
	hashes := make([]string, 0, len(commits))
	index := make(map[string]int, len(commits))
	for i, commit := range commits {
		hashes = append(hashes, commit.Hash)
		index[commit.Hash] = i
	}
	for _, chunk := range chunkStrings(hashes, 500) {
		args := make([]any, 0, len(chunk))
		holders := make([]string, 0, len(chunk))
		for _, hash := range chunk {
			holders = append(holders, "?")
			args = append(args, hash)
		}
		query := fmt.Sprintf(
			`SELECT commit_hash, parent_hash
			 FROM commit_parents
			 WHERE commit_hash IN (%s)
			 ORDER BY commit_hash ASC, parent_index ASC`,
			strings.Join(holders, ","),
		)
		rows, err := db.sql.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("load parents: %w", err)
		}
		for rows.Next() {
			var commitHash, parentHash string
			if err := rows.Scan(&commitHash, &parentHash); err != nil {
				rows.Close()
				return fmt.Errorf("scan parent row: %w", err)
			}
			commits[index[commitHash]].ParentHashes = append(commits[index[commitHash]].ParentHashes, parentHash)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close parent rows: %w", err)
		}
	}
	return nil
}

func scanChannel(scanner interface{ Scan(dest ...any) error }) (model.Channel, error) {
	var channel model.Channel
	var createdAt string
	var createdBy sql.NullString
	if err := scanner.Scan(&channel.Name, &createdBy, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Channel{}, ErrNotFound
		}
		return model.Channel{}, fmt.Errorf("scan channel: %w", err)
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return model.Channel{}, err
	}
	channel.CreatedAt = parsed
	if createdBy.Valid {
		channel.CreatedByAgentID = &createdBy.String
	}
	return channel, nil
}

func scanPost(scanner interface{ Scan(dest ...any) error }) (model.Post, error) {
	var post model.Post
	var createdAt string
	var parent sql.NullInt64
	if err := scanner.Scan(&post.ID, &post.ChannelName, &post.AgentID, &post.Body, &createdAt, &parent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Post{}, ErrNotFound
		}
		return model.Post{}, fmt.Errorf("scan post: %w", err)
	}
	parsed, err := parseTime(createdAt)
	if err != nil {
		return model.Post{}, err
	}
	post.CreatedAt = parsed
	if parent.Valid {
		post.ParentPostID = &parent.Int64
	}
	return post, nil
}

func scanCommit(scanner interface{ Scan(dest ...any) error }) (model.Commit, error) {
	var commit model.Commit
	var createdAt, commitTime string
	var agentID sql.NullString
	if err := scanner.Scan(
		&commit.Hash,
		&agentID,
		&commit.AuthorName,
		&commit.AuthorEmail,
		&commit.CommitterName,
		&commit.CommitterEmail,
		&commit.Subject,
		&commit.Body,
		&createdAt,
		&commitTime,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Commit{}, ErrNotFound
		}
		return model.Commit{}, fmt.Errorf("scan commit: %w", err)
	}
	if agentID.Valid {
		commit.AgentID = &agentID.String
	}
	var err error
	commit.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.Commit{}, err
	}
	commit.CommitTime, err = parseTime(commitTime)
	if err != nil {
		return model.Commit{}, err
	}
	return commit, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	t, err := time.Parse(timeLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", value, err)
	}
	return t.UTC(), nil
}

func nullString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func firstNonNil(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func chunkStrings(values []string, size int) [][]string {
	if len(values) == 0 {
		return nil
	}
	var chunks [][]string
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		chunks = append(chunks, values[start:end])
	}
	return chunks
}

func isConstraintError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "constraint failed") || strings.Contains(msg, "UNIQUE constraint failed")
}
