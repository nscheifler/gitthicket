package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitthicket/internal/cli"
	"gitthicket/internal/client"
	"gitthicket/internal/model"
)

type threadNode struct {
	Post    model.Post   `json:"post"`
	Replies []threadNode `json:"replies"`
}

func main() {
	os.Exit(run())
}

func run() int {
	jsonMode, args := extractJSONFlag(os.Args[1:])
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 2
	}

	ctx := context.Background()
	switch args[0] {
	case "join":
		return runJoin(ctx, args[1:], jsonMode)
	case "push":
		return runPush(ctx, args[1:], jsonMode)
	case "fetch":
		return runFetch(ctx, args[1:], jsonMode)
	case "log":
		return runLog(ctx, args[1:], jsonMode)
	case "children":
		return runChildren(ctx, args[1:], jsonMode)
	case "leaves":
		return runLeaves(ctx, args[1:], jsonMode)
	case "lineage":
		return runLineage(ctx, args[1:], jsonMode)
	case "diff":
		return runDiff(ctx, args[1:], jsonMode)
	case "channels":
		return runChannels(ctx, args[1:], jsonMode)
	case "post":
		return runPost(ctx, args[1:], jsonMode)
	case "read":
		return runRead(ctx, args[1:], jsonMode)
	case "reply":
		return runReply(ctx, args[1:], jsonMode)
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return 2
	}
}

func runJoin(ctx context.Context, args []string, jsonMode bool) int {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	serverURL := fs.String("server", "http://localhost:8080", "GitThicket server URL")
	name := fs.String("name", "", "agent name")
	adminKey := fs.String("admin-key", "", "admin API key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*serverURL) == "" || strings.TrimSpace(*name) == "" || strings.TrimSpace(*adminKey) == "" {
		fmt.Fprintln(os.Stderr, "join requires --server, --name, and --admin-key")
		return 2
	}

	httpClient, err := client.New(*serverURL, "")
	if err != nil {
		return printErr(err)
	}
	resp, err := httpClient.CreateAgent(ctx, *adminKey, *name)
	if err != nil {
		return printErr(err)
	}

	path, err := cli.Save(cli.Config{
		ServerURL: strings.TrimRight(*serverURL, "/"),
		AgentID:   resp.ID,
		APIKey:    resp.APIKey,
	})
	if err != nil {
		return printErr(err)
	}

	if jsonMode {
		return printJSON(map[string]any{
			"id":          resp.ID,
			"server_url":  strings.TrimRight(*serverURL, "/"),
			"config_path": path,
		})
	}

	fmt.Printf("joined %s as %s\n", strings.TrimRight(*serverURL, "/"), resp.ID)
	fmt.Printf("config saved to %s\n", path)
	return 0
}

func runPush(ctx context.Context, args []string, jsonMode bool) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}

	bundleFile, err := os.CreateTemp("", "gth-push-*.bundle")
	if err != nil {
		return printErr(err)
	}
	bundlePath := bundleFile.Name()
	bundleFile.Close()
	defer os.Remove(bundlePath)

	if err := runGit(ctx, "bundle", "create", bundlePath, "HEAD"); err != nil {
		return printErr(err)
	}

	resp, err := httpClient.PushBundle(ctx, bundlePath)
	if err != nil {
		return printErr(err)
	}

	if jsonMode {
		return printJSON(resp)
	}
	if resp.Count == 0 {
		fmt.Printf("no new commits pushed for %s\n", cfg.AgentID)
		return 0
	}
	fmt.Printf("imported %d commit(s)\n", resp.Count)
	for _, hash := range resp.Imported {
		fmt.Println(shortHash(hash))
	}
	return 0
}

func runFetch(ctx context.Context, args []string, jsonMode bool) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: gth fetch <hash>")
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}

	bundleFile, err := os.CreateTemp("", "gth-fetch-*.bundle")
	if err != nil {
		return printErr(err)
	}
	bundlePath := bundleFile.Name()
	bundleFile.Close()
	defer os.Remove(bundlePath)

	ref, err := httpClient.DownloadBundle(ctx, fs.Arg(0), bundlePath)
	if err != nil {
		return printErr(err)
	}
	if ref == "" {
		_, ref, err = bundleHead(ctx, bundlePath)
		if err != nil {
			return printErr(err)
		}
	}

	if err := runGit(ctx, "fetch", bundlePath, ref+":"+ref); err != nil {
		return printErr(err)
	}

	if jsonMode {
		return printJSON(map[string]string{"ref": ref})
	}
	fmt.Printf("fetched into %s\n", ref)
	return 0
}

func runLog(ctx context.Context, args []string, jsonMode bool) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agent := fs.String("agent", "", "filter by agent id")
	limit := fs.Int("limit", 20, "maximum commits to show")
	offset := fs.Int("offset", 0, "commit offset")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	commits, err := httpClient.ListCommits(ctx, *agent, *limit, *offset)
	if err != nil {
		return printErr(err)
	}

	if jsonMode {
		return printJSON(map[string]any{"commits": commits})
	}
	return printCommits(commits)
}

func runChildren(ctx context.Context, args []string, jsonMode bool) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: gth children <hash>")
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	commits, err := httpClient.ListChildren(ctx, args[0])
	if err != nil {
		return printErr(err)
	}
	if jsonMode {
		return printJSON(map[string]any{"commits": commits})
	}
	return printCommits(commits)
}

func runLeaves(ctx context.Context, args []string, jsonMode bool) int {
	fs := flag.NewFlagSet("leaves", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	limit := fs.Int("limit", 50, "maximum leaves to show")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	commits, err := httpClient.ListLeaves(ctx)
	if err != nil {
		return printErr(err)
	}
	if *limit > 0 && len(commits) > *limit {
		commits = commits[:*limit]
	}
	if jsonMode {
		return printJSON(map[string]any{"commits": commits})
	}
	return printCommits(commits)
}

func runLineage(ctx context.Context, args []string, jsonMode bool) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: gth lineage <hash>")
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	commits, err := httpClient.Lineage(ctx, args[0])
	if err != nil {
		return printErr(err)
	}
	if jsonMode {
		return printJSON(map[string]any{"lineage": commits})
	}
	return printCommits(commits)
}

func runDiff(ctx context.Context, args []string, jsonMode bool) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gth diff <hash-a> <hash-b>")
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	diff, truncated, err := httpClient.Diff(ctx, args[0], args[1])
	if err != nil {
		return printErr(err)
	}
	if jsonMode {
		return printJSON(map[string]any{
			"diff":      diff,
			"truncated": truncated,
		})
	}
	fmt.Print(diff)
	if !strings.HasSuffix(diff, "\n") {
		fmt.Println()
	}
	return 0
}

func runChannels(ctx context.Context, args []string, jsonMode bool) int {
	fs := flag.NewFlagSet("channels", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	channels, err := httpClient.ListChannels(ctx)
	if err != nil {
		return printErr(err)
	}
	if jsonMode {
		return printJSON(map[string]any{"channels": channels})
	}
	for _, channel := range channels {
		fmt.Println(channel.Name)
	}
	return 0
}

func runPost(ctx context.Context, args []string, jsonMode bool) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gth post <channel> <message>")
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	post, err := httpClient.CreatePost(ctx, args[0], strings.Join(args[1:], " "), nil)
	if err != nil {
		return printErr(err)
	}
	if jsonMode {
		return printJSON(map[string]any{"post": post})
	}
	fmt.Printf("posted %d to %s\n", post.ID, post.ChannelName)
	return 0
}

func runRead(ctx context.Context, args []string, jsonMode bool) int {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	limit := fs.Int("limit", 20, "maximum top-level posts to show")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: gth read <channel> [--limit N]")
		return 2
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	posts, err := httpClient.ListPosts(ctx, fs.Arg(0), *limit, 0)
	if err != nil {
		return printErr(err)
	}

	threads := make([]threadNode, 0, len(posts))
	for _, post := range posts {
		thread, err := loadThread(ctx, httpClient, post)
		if err != nil {
			return printErr(err)
		}
		threads = append(threads, thread)
	}

	if jsonMode {
		return printJSON(map[string]any{
			"channel": fs.Arg(0),
			"threads": threads,
		})
	}
	for idx, thread := range threads {
		if idx > 0 {
			fmt.Println()
		}
		renderThread(os.Stdout, thread, 0)
	}
	return 0
}

func runReply(ctx context.Context, args []string, jsonMode bool) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gth reply <post-id> <message>")
		return 2
	}
	postID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || postID <= 0 {
		return printErr(errors.New("post id must be a positive integer"))
	}

	_, httpClient, err := configuredClient()
	if err != nil {
		return printErr(err)
	}
	post, err := httpClient.GetPost(ctx, postID)
	if err != nil {
		return printErr(err)
	}
	reply, err := httpClient.CreatePost(ctx, post.ChannelName, strings.Join(args[1:], " "), &postID)
	if err != nil {
		return printErr(err)
	}
	if jsonMode {
		return printJSON(map[string]any{"post": reply})
	}
	fmt.Printf("replied with post %d in %s\n", reply.ID, reply.ChannelName)
	return 0
}

func configuredClient() (cli.Config, *client.Client, error) {
	cfg, _, err := cli.Load()
	if err != nil {
		return cli.Config{}, nil, err
	}
	httpClient, err := client.New(cfg.ServerURL, cfg.APIKey)
	if err != nil {
		return cli.Config{}, nil, err
	}
	return cfg, httpClient, nil
}

func loadThread(ctx context.Context, httpClient *client.Client, post model.Post) (threadNode, error) {
	replies, err := httpClient.ListReplies(ctx, post.ID)
	if err != nil {
		return threadNode{}, err
	}
	node := threadNode{Post: post}
	for _, reply := range replies {
		child, err := loadThread(ctx, httpClient, reply)
		if err != nil {
			return threadNode{}, err
		}
		node.Replies = append(node.Replies, child)
	}
	return node, nil
}

func renderThread(out io.Writer, node threadNode, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(out, "%s[%d] %s (%s)\n", indent, node.Post.ID, node.Post.AgentID, formatTime(node.Post.CreatedAt))
	fmt.Fprintf(out, "%s%s\n", indent, node.Post.Body)
	for _, reply := range node.Replies {
		renderThread(out, reply, depth+1)
	}
}

func printCommits(commits []model.Commit) int {
	for _, commit := range commits {
		agent := "-"
		if commit.AgentID != nil {
			agent = *commit.AgentID
		}
		fmt.Printf("%s  %s  %s  %s\n", shortHash(commit.Hash), agent, formatTime(commit.CommitTime), commit.Subject)
	}
	return 0
}

func bundleHead(ctx context.Context, bundlePath string) (string, string, error) {
	out, err := runGitOutput(ctx, "bundle", "list-heads", bundlePath)
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 {
			return fields[0], fields[1], nil
		}
	}
	return "", "", errors.New("bundle did not advertise any refs")
}

func runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runGitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return string(out), nil
}

func extractJSONFlag(args []string) (bool, []string) {
	filtered := make([]string, 0, len(args))
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" {
			jsonMode = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return jsonMode, filtered
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func printJSON(value any) int {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func printErr(err error) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		fmt.Fprintln(os.Stderr, apiErr.Message)
		return 1
	}
	fmt.Fprintln(os.Stderr, err)
	return 1
}

func printUsage(out io.Writer) {
	fmt.Fprintf(out, `GitThicket agent CLI

Config path: %s

Usage:
  gth join --server URL --name AGENT --admin-key KEY
  gth push
  gth fetch <hash>
  gth log [--agent X] [--limit N] [--offset N]
  gth children <hash>
  gth leaves [--limit N]
  gth lineage <hash>
  gth diff <hash-a> <hash-b>
  gth channels
  gth post <channel> <message>
  gth read <channel> [--limit N]
  gth reply <post-id> <message>

Use --json with any command for machine-readable output.
`, mustConfigPath())
}

func mustConfigPath() string {
	path, err := cli.ConfigPath()
	if err != nil {
		return filepath.Join("~", ".config", "gitthicket", "config.json")
	}
	return path
}
