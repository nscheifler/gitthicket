package model

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

type Agent struct {
	ID         string     `json:"id"`
	APIKeyHash string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
}

type Channel struct {
	Name             string    `json:"name"`
	CreatedByAgentID *string   `json:"created_by_agent_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type Post struct {
	ID           int64     `json:"id"`
	ChannelName  string    `json:"channel_name"`
	AgentID      string    `json:"agent_id"`
	Body         string    `json:"body"`
	CreatedAt    time.Time `json:"created_at"`
	ParentPostID *int64    `json:"parent_post_id,omitempty"`
}

type Commit struct {
	Hash           string    `json:"hash"`
	AgentID        *string   `json:"agent_id,omitempty"`
	AuthorName     string    `json:"author_name"`
	AuthorEmail    string    `json:"author_email"`
	CommitterName  string    `json:"committer_name"`
	CommitterEmail string    `json:"committer_email"`
	Subject        string    `json:"subject"`
	Body           string    `json:"body,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	CommitTime     time.Time `json:"commit_time"`
	ParentHashes   []string  `json:"parents,omitempty"`
}

var (
	channelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	agentPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func NormalizeChannelName(name string) (string, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return "", errors.New("channel name is required")
	}
	name = strings.Join(strings.Fields(name), "-")
	if !channelPattern.MatchString(name) {
		return "", errors.New("channel names must match [a-z0-9][a-z0-9_-]{0,63}")
	}
	return name, nil
}

func ValidateAgentID(id string) error {
	if !agentPattern.MatchString(id) {
		return errors.New("agent id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
	}
	return nil
}
