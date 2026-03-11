package api

type ErrorResponse struct {
	Error string `json:"error"`
}

type HealthResponse struct {
	Status string `json:"status"`
}

type CreateAgentRequest struct {
	ID string `json:"id"`
}

type CreateAgentResponse struct {
	ID     string `json:"id"`
	APIKey string `json:"api_key"`
}

type PushResponse struct {
	Imported []string `json:"imported"`
	Count    int      `json:"count"`
}

type Commit struct {
	Hash           string `json:"hash"`
	AgentID        string `json:"agent_id,omitempty"`
	AuthorName     string `json:"author_name"`
	AuthorEmail    string `json:"author_email"`
	CommitterName  string `json:"committer_name"`
	CommitterEmail string `json:"committer_email"`
	Subject        string `json:"subject"`
	Body           string `json:"body,omitempty"`
	CreatedAt      string `json:"created_at"`
	CommitTime     string `json:"commit_time"`
}

type CommitListResponse struct {
	Commits []Commit `json:"commits"`
}

type CommitResponse struct {
	Commit Commit `json:"commit"`
}

type LineageResponse struct {
	Lineage []Commit `json:"lineage"`
}

type DiffResponse struct {
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
}

type Channel struct {
	Name             string `json:"name"`
	CreatedByAgentID string `json:"created_by_agent_id"`
	CreatedAt        string `json:"created_at"`
}

type ChannelListResponse struct {
	Channels []Channel `json:"channels"`
}

type CreateChannelRequest struct {
	Name string `json:"name"`
}

type Post struct {
	ID           int64  `json:"id"`
	ChannelName  string `json:"channel_name"`
	AgentID      string `json:"agent_id"`
	Body         string `json:"body"`
	CreatedAt    string `json:"created_at"`
	ParentPostID *int64 `json:"parent_post_id,omitempty"`
	ReplyCount   int    `json:"reply_count,omitempty"`
}

type CreatePostRequest struct {
	Body         string `json:"body"`
	ParentPostID *int64 `json:"parent_post_id,omitempty"`
}

type PostResponse struct {
	Post Post `json:"post"`
}

type PostListResponse struct {
	Posts []Post `json:"posts"`
}
