package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

const (
	// agentCommentMarker identifies comments written by this agent, so previous
	// agent reviews can be recognized in the merge request history.
	agentCommentMarker = "<!-- AGENT_COMMENT -->"

	// mrHistoryNoteMaxChars limits the size of a single comment injected as
	// part of the merge request history.
	mrHistoryNoteMaxChars = 15000

	// mrHistoryNotesPerPage is the number of comments fetched with a single
	// request when building the merge request history.
	mrHistoryNotesPerPage = 100
)

func main() {
	ctx := context.Background()

	// setup configuration from environment variables with defaults
	baseURL := envOrFail("OPENAI_BASE_URL")
	apiKey := envOrFail("OPENAI_API_KEY")
	modelName := envOrFail("MODEL_NAME")
	repoPath := envOrFail("REPO_PATH")
	targetBranch := envStringOrDefault("TARGET_BRANCH", "main")
	fetchAllowedDomains := envStringOrDefault("FETCH_ALLOWED_DOMAINS", "github.com,githubusercontent.com")
	allowedDomains := strings.Split(fetchAllowedDomains, ",")
	for i := range allowedDomains {
		allowedDomains[i] = strings.TrimSpace(allowedDomains[i])
	}

	systemPrompt := os.Getenv("SYSTEM_PROMPT")
	if systemPrompt == "" {
		systemPrompt = fmt.Sprintf("You are an automated code review system. The repo is in %s. Use the 'filesystem' tools to inspect files in the %s directory, the 'run_command' shell tool for read-only commands such as git, rg, ls, cat, sed, and find, and the 'fetch' tool to fetch content from allowed domains (%s).", repoPath, repoPath, fetchAllowedDomains)
	}

	// Read task from file if TASK_FILE is set, otherwise use TASK environment variable
	taskPrompt := os.Getenv("TASK")
	if taskFile := os.Getenv("TASK_FILE"); taskFile != "" {
		content, err := os.ReadFile(taskFile)
		if err != nil {
			fatalf("failed to read task file %s: %v", taskFile, err)
		}
		taskPrompt = string(content)
		slog.Info("Using task from file", "file", taskFile)
	}

	if taskPrompt == "" {
		fatalf("no task provided: set TASK environment variable or provide a file with TASK_FILE")
	}

	// Replace __TARGET_BRANCH__ placeholder with actual target branch value
	taskPrompt = strings.ReplaceAll(taskPrompt, "__TARGET_BRANCH__", targetBranch)
	slog.Info("Using target branch", "branch", targetBranch)

	// Append the existing merge request history (description and previous
	// comments) so the agent can take earlier reviews into account.
	if mrHistory := loadMergeRequestHistory(ctx); mrHistory != "" {
		taskPrompt = fmt.Sprintf("%s\n\n---\n\n%s", taskPrompt, mrHistory)
	}

	maxSteps := envIntOrDefault("MAX_STEPS", 200)
	maxToolResultSize := envIntOrDefault("MAX_TOOL_RESULT_SIZE", 30000) // 30KB default

	level := slog.LevelInfo
	if logLevel := os.Getenv("LOG_LEVEL"); logLevel != "" {
		err := level.UnmarshalText([]byte(logLevel))
		if err != nil {
			fatalf("failed to parse LOG_LEVEL: %v", err)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	oai := openai.NewClient(option.WithAPIKey(apiKey), option.WithBaseURL(baseURL))

	// setup MCP clients for the filesystem, shell servers
	impl := &mcp.Implementation{Name: "go-review-agent", Version: "1.0.0"}

	slog.Info("Starting filesystem MCP client")
	fsClient := mcp.NewClient(impl, nil)
	fsTransport := &mcp.CommandTransport{Command: exec.Command("/app/mcp-filesystem-server", repoPath)}
	fsSession, err := fsClient.Connect(ctx, fsTransport, nil)
	if err != nil {
		fatalf("failed to start filesystem client: %v", err)
	}
	defer func() {
		if err := fsSession.Close(); err != nil {
			slog.Error("failed to close filesystem session", "error", err)
		}
	}()

	slog.Info("Starting shell MCP client")
	shellClient := mcp.NewClient(impl, nil)
	shellTransport := &mcp.CommandTransport{Command: exec.Command("/app/shellmcp", repoPath)}
	shellSession, err := shellClient.Connect(ctx, shellTransport, nil)
	if err != nil {
		fatalf("failed to start shell client: %v", err)
	}
	defer func() {
		if err := shellSession.Close(); err != nil {
			slog.Error("failed to close shell session", "error", err)
		}
	}()

	slog.Info("Starting fetch MCP client")
	fetchClient := mcp.NewClient(impl, nil)
	fetchTransport := &mcp.CommandTransport{Command: exec.Command("/app/mcp-fetch", "server")}
	fetchSession, err := fetchClient.Connect(ctx, fetchTransport, nil)
	if err != nil {
		fatalf("failed to start fetch client: %v", err)
	}
	defer func() {
		if err := fetchSession.Close(); err != nil {
			slog.Error("failed to close fetch session", "error", err)
		}
	}()

	// initialize MCP clients and fetch tool lists
	slog.Info("Connecting to MCP servers")

	// Add timeout for MCP initialization to prevent hanging
	initTimeout := 30 * time.Second
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	slog.Info("Fetching tool lists from MCP servers")
	fsToolsRes, err := fsSession.ListTools(initCtx, &mcp.ListToolsParams{})
	if err != nil {
		fatalf("failed to load filesystem tools: %v", err)
	}
	shellToolsRes, err := shellSession.ListTools(initCtx, &mcp.ListToolsParams{})
	if err != nil {
		fatalf("failed to load shell tools: %v", err)
	}
	fetchToolsRes, err := fetchSession.ListTools(initCtx, &mcp.ListToolsParams{})
	if err != nil {
		fatalf("failed to load fetch tools: %v", err)
	}

	// build OpenAI tool definitions from the MCP tool lists
	allMCPTools := append(fsToolsRes.Tools, shellToolsRes.Tools...)
	allMCPTools = append(allMCPTools, fetchToolsRes.Tools...)
	oaiTools := buildOpenAITools(allMCPTools)

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(taskPrompt),
	}

	startedAt := time.Now()
	stopReason := "max_steps_reached"
	finalAnswer := ""
	emptyResponseRetries := 0
	maxEmptyResponseRetries := 2

	// main agent loop
	slog.Info("Starting agent loop", "max_steps", maxSteps)

	for range maxSteps {
		resp, err := oai.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:      openai.ChatModel(modelName),
			Messages:   messages,
			Tools:      oaiTools,
			ToolChoice: openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("auto")},
		})
		if err != nil {
			fatalf("LLM request failed: %v", err)
		}

		if len(resp.Choices) == 0 {
			stopReason = "no_model_response"
			break
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg.ToParam())

		if len(msg.ToolCalls) == 0 {
			if strings.TrimSpace(msg.Content) == "" {
				if emptyResponseRetries < maxEmptyResponseRetries {
					slog.Warn("Received empty response, requesting retry",
						"retry", emptyResponseRetries+1,
						"max_retries", maxEmptyResponseRetries)
					messages = append(messages, openai.UserMessage(
						"Your response was empty. Please provide a detailed answer to the original task.",
					))
					emptyResponseRetries++
					continue
				} else {
					slog.Error("Max empty response retries reached, giving up")
					stopReason = "empty_response"
					break
				}
			}

			// Check if the LLM tried to make tool calls in text format
			if containsToolCallPatterns(msg.Content) {
				slog.Warn("Detected tool call patterns in text response, requesting proper tool call format")
				messages = append(messages, openai.UserMessage(
					"I noticed you tried to make tool calls in your response text. Please use the proper tool calling API instead. Please try again.",
				))
				continue
			}

			stopReason = "final_response"
			finalAnswer = msg.Content
			break
		}

		prettyLogReasoning(resp)

		for _, tc := range msg.ToolCalls {
			resultText := executeToolCall(ctx, tc, maxToolResultSize, fsToolsRes.Tools, shellToolsRes.Tools, fetchToolsRes.Tools, fsSession, shellSession, fetchSession, allowedDomains)
			slog.Debug("Tool call result", "tool", tc.Function.Name, "result", resultText)
			messages = append(messages, openai.ToolMessage(resultText, tc.ID))
		}
	}

	switch stopReason {
	case "final_response":
		slog.Info("Final model response")
		fmt.Println("============ FINAL ANSWER ============")
		fmt.Println(finalAnswer)
		// Post the final answer as a comment to GitLab MR if configured
		if err := postGitLabMRComment(ctx, finalAnswer); err != nil {
			slog.Error("Failed to post GitLab MR comment", "error", err)
		}
	case "no_model_response":
		slog.Info("No response received from the model")
	case "empty_response":
		slog.Error("Agent failed: received empty response after multiple retries")
	default:
		slog.Info("Reached max steps without a final answer. Increase MAX_STEPS if needed", "max_steps", maxSteps)
	}

	duration := time.Since(startedAt).Round(time.Millisecond)
	slog.Info("Run summary", "stop_reason", stopReason, "duration", duration)
}

// Note: this is necessary because openai-go does not currently support direct access to ReasoningContent, which is needed for pretty logging of the model's thought process. Can be removed  once openai-go supports it natively.
func prettyLogReasoning(resp *openai.ChatCompletion) {
	var rawResponse struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}

	// slog.Info("Raw response JSON", "msg", resp.RawJSON())
	if err := json.Unmarshal([]byte(resp.RawJSON()), &rawResponse); err == nil {
		if len(rawResponse.Choices) > 0 && rawResponse.Choices[0].Message.ReasoningContent != "" {
			thinkingSteps := rawResponse.Choices[0].Message.ReasoningContent
			// convert escaped newlines into real newlines for readability
			formatted := strings.ReplaceAll(thinkingSteps, `\n`, "\n")
			formatted = strings.TrimSpace(formatted)
			if formatted != "" {
				// log to stderr for human-readable output
				// found no way to log multiple lines with slog, so using fmt for the formatted reasoning steps
				slog.Info("model reasoning:")
				fmt.Fprintln(os.Stderr, formatted)
			}
		}
	} else {
		slog.Debug("Could not unmarshal raw JSON for reasoning_content", "error", err)
	}
}

func buildOpenAITools(tools []*mcp.Tool) []openai.ChatCompletionToolUnionParam {
	result := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))

	for _, tool := range tools {
		schema := shared.FunctionParameters{
			"type":       "object",
			"properties": map[string]any{},
		}

		// In go-sdk v1.x, Tool.InputSchema is typed as `any`; when tools are
		// listed from a client it holds the JSON-marshaled schema as map[string]any.
		if input, ok := tool.InputSchema.(map[string]any); ok {
			if t, ok := input["type"].(string); ok && t != "" {
				schema["type"] = t
			}
			if props, ok := input["properties"]; ok && props != nil {
				schema["properties"] = props
			}
			if req, ok := input["required"].([]any); ok && len(req) > 0 {
				schema["required"] = req
			}
		}

		result = append(result, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        tool.Name,
			Description: openai.String(tool.Description),
			Parameters:  schema,
		}))
	}

	return result
}

func executeToolCall(ctx context.Context, tc openai.ChatCompletionMessageToolCallUnion, maxToolResultSize int, fsTools []*mcp.Tool, shellTools []*mcp.Tool, fetchTools []*mcp.Tool, fsSession *mcp.ClientSession, shellSession *mcp.ClientSession, fetchSession *mcp.ClientSession, allowedDomains []string) string {
	argsJSON := tc.Function.Arguments
	toolName := tc.Function.Name
	startedAt := time.Now()

	var parsed map[string]any
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
			return fmt.Sprintf("failed to parse tool arguments: %v", err)
		}
	}
	if parsed == nil {
		parsed = map[string]any{}
	}

	toolSummary := summarizeToolCall(toolName, parsed)
	slog.Info("Run", "tool", toolSummary, "tool_name", toolName, "tool_call_id", tc.ID)

	// Validate fetch tool URLs before execution
	if hasTool(fetchTools, toolName) {
		urlStr, ok := parsed["url"].(string)
		if !ok || strings.TrimSpace(urlStr) == "" {
			return "fetch tool call rejected: missing or invalid 'url' argument"
		}
		if !isURLAllowed(urlStr, allowedDomains) {
			slog.Warn("Fetch URL not allowed", "url", urlStr, "allowed_domains", allowedDomains)
			return fmt.Sprintf("fetch URL not allowed: %s (allowed domains: %s)", urlStr, strings.Join(allowedDomains, ", "))
		}
	}

	var (
		callResult *mcp.CallToolResult
		err        error
	)

	if hasTool(fsTools, toolName) {
		callResult, err = fsSession.CallTool(ctx, &mcp.CallToolParams{
			Name:      toolName,
			Arguments: json.RawMessage(argsJSON),
		})
	} else if hasTool(shellTools, toolName) {
		callResult, err = shellSession.CallTool(ctx, &mcp.CallToolParams{
			Name:      toolName,
			Arguments: json.RawMessage(argsJSON),
		})
	} else if hasTool(fetchTools, toolName) {
		callResult, err = fetchSession.CallTool(ctx, &mcp.CallToolParams{
			Name:      toolName,
			Arguments: json.RawMessage(argsJSON),
		})
	} else {
		return fmt.Sprintf("tool not found on any MCP server: %s", toolName)
	}

	if err != nil {
		slog.Error("Tool execution failed", "tool", toolName, "tool_call_id", tc.ID, "duration", time.Since(startedAt).Round(time.Millisecond), "error", err)
		return fmt.Sprintf("tool execution failed: %v", err)
	}

	if callResult != nil && callResult.IsError {
		slog.Error("Tool returned error result", "tool", toolName, "tool_call_id", tc.ID, "duration", time.Since(startedAt).Round(time.Millisecond), "result_preview", summarizeCallToolResult(callResult, 300))
	} else {
		slog.Info("Tool execution succeeded", "tool", toolName, "tool_call_id", tc.ID, "duration", time.Since(startedAt).Round(time.Millisecond))
	}

	payload, err := json.MarshalIndent(callResult, "", "  ")
	if err != nil {
		return fmt.Sprintf("tool returned result, but marshal failed: %v", err)
	}

	result := string(payload)
	result = truncateToolResult(result, maxToolResultSize)

	return result
}

// mrHistoryEntry is a single pre-rendered comment of the merge request history.
type mrHistoryEntry struct {
	meta     string // author, timestamp and status flags
	position string // optional "<path>:<line>" for comments on changed lines
	body     string
}

// render formats the entry. The comment number is added here because it is not
// counted towards the size limit.
func (e mrHistoryEntry) render(commentNumber int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "### Comment %d — %s\n", commentNumber, e.meta)
	if e.position != "" {
		fmt.Fprintf(&b, "%s\n", e.position)
	}
	fmt.Fprintf(&b, "%s\n\n", e.body)

	return b.String()
}

// size estimates the rendered size of the entry.
func (e mrHistoryEntry) size() int {
	const markdownOverhead = 32 // headings, newlines and the comment counter
	return len(e.meta) + len(e.position) + len(e.body) + markdownOverhead
}

// formatMergeRequestHistory renders the merge request context and the previous
// comments as a markdown block that is appended to the task prompt. The notes
// are expected to be ordered newest first, the output is chronological.
func formatMergeRequestHistory(mr *gitlab.MergeRequest, notes []*gitlab.Note, maxChars int, includeSystemNotes bool) string {
	var b strings.Builder

	if mr != nil {
		b.WriteString("## Merge request context\n\n")
		fmt.Fprintf(&b, "- Title: %s\n", strings.TrimSpace(mr.Title))
		fmt.Fprintf(&b, "- State: %s\n", mr.State)
		fmt.Fprintf(&b, "- Source branch: %s\n", mr.SourceBranch)
		fmt.Fprintf(&b, "- Target branch: %s\n", mr.TargetBranch)
		if mr.Author != nil && mr.Author.Username != "" {
			fmt.Fprintf(&b, "- Author: @%s\n", mr.Author.Username)
		}
		if mr.WebURL != "" {
			fmt.Fprintf(&b, "- URL: %s\n", mr.WebURL)
		}
		if description := strings.TrimSpace(mr.Description); description != "" {
			fmt.Fprintf(&b, "\n### Description\n\n%s\n", description)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Previous comments (chronological)\n\n")

	entries := collectMergeRequestHistoryEntries(notes, includeSystemNotes)

	selected := make([]mrHistoryEntry, 0, len(entries))
	omitted := 0
	remaining := maxChars
	for i, entry := range entries {
		if entry.size() > remaining {
			// The newest comment is always kept, truncated if it is too large.
			if len(selected) == 0 {
				entry.body = truncateHistoryText(entry.body, max(remaining-len(entry.meta)-len(entry.position), 200))
				selected = append(selected, entry)
				omitted = len(entries) - i - 1
			} else {
				omitted = len(entries) - i
			}
			break
		}
		selected = append(selected, entry)
		remaining -= entry.size()
	}

	if len(selected) == 0 {
		b.WriteString("(no previous comments)\n")
		return b.String()
	}

	if omitted > 0 {
		fmt.Fprintf(&b, "[... %d older comment(s) omitted, MR_HISTORY_MAX_CHARS=%d ...]\n\n", omitted, maxChars)
	}

	// The entries are stored newest first, print them chronologically.
	for i := len(selected) - 1; i >= 0; i-- {
		b.WriteString(selected[i].render(len(selected) - i))
	}

	return b.String()
}

// collectMergeRequestHistoryEntries filters and normalizes the comments. The
// notes are expected to be ordered newest first.
func collectMergeRequestHistoryEntries(notes []*gitlab.Note, includeSystemNotes bool) []mrHistoryEntry {
	entries := make([]mrHistoryEntry, 0, len(notes))

	for _, note := range notes {
		if note == nil || (note.System && !includeSystemNotes) {
			continue
		}

		// The marker is not part of the review itself.
		body := strings.TrimSpace(strings.ReplaceAll(note.Body, agentCommentMarker, ""))
		if body == "" {
			continue
		}

		entries = append(entries, mrHistoryEntry{
			meta:     formatNoteMeta(note),
			position: formatNotePosition(note),
			body:     truncateHistoryText(body, mrHistoryNoteMaxChars),
		})
	}

	return entries
}

// formatNoteMeta summarizes author, timestamp and status flags of a comment.
func formatNoteMeta(note *gitlab.Note) string {
	author := "unknown author"
	if note.Author.Username != "" {
		author = "@" + note.Author.Username
	}

	flags := make([]string, 0, 4)
	if note.System {
		flags = append(flags, "system")
	}
	if note.Resolvable {
		if note.Resolved {
			flags = append(flags, "resolved")
		} else {
			flags = append(flags, "unresolved")
		}
	}
	if note.Internal {
		flags = append(flags, "internal")
	}
	if strings.Contains(note.Body, agentCommentMarker) {
		flags = append(flags, "previous agent review, will be replaced by the new review")
	}

	meta := author
	if note.CreatedAt != nil {
		meta += " " + note.CreatedAt.UTC().Format(time.RFC3339)
	}
	if len(flags) > 0 {
		meta += " (" + strings.Join(flags, ", ") + ")"
	}

	return meta
}

// formatNotePosition returns the file and line of a comment on a changed line.
func formatNotePosition(note *gitlab.Note) string {
	if note.Position == nil {
		return ""
	}

	path := note.Position.NewPath
	line := note.Position.NewLine
	if path == "" {
		path = note.Position.OldPath
		line = note.Position.OldLine
	}

	switch {
	case path != "" && line > 0:
		return fmt.Sprintf("`%s:%d`", path, line)
	case path != "":
		return fmt.Sprintf("`%s`", path)
	default:
		return ""
	}
}

// truncateHistoryText shortens a single comment for the injected history. In
// contrast to truncateToolResult the beginning of the text is kept, because the
// important part of a review comment is usually at the top.
func truncateHistoryText(text string, maxSize int) string {
	if len(text) <= maxSize {
		return text
	}

	const notice = " [... truncated ...]"
	if maxSize <= len(notice) {
		return text[:maxSize]
	}

	return strings.TrimSpace(text[:maxSize-len(notice)]) + notice
}

func summarizeCallToolResult(res *mcp.CallToolResult, maxLen int) string {
	if res == nil {
		return ""
	}

	payload, err := json.Marshal(res)
	if err != nil {
		return ""
	}

	s := strings.TrimSpace(string(payload))
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}

	if maxLen <= 3 {
		return s[:maxLen]
	}

	return s[:maxLen-3] + "..."
}

// isURLAllowed checks if a URL's domain is in the allowed domains list
func isURLAllowed(urlStr string, allowedDomains []string) bool {
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return false
	}

	host := parsedURL.Hostname()
	if host == "" {
		return false
	}

	for _, allowedDomain := range allowedDomains {
		// Exact match
		if host == allowedDomain {
			return true
		}
		// Subdomain match (e.g., raw.githubusercontent.com matches githubusercontent.com)
		if strings.HasSuffix(host, "."+allowedDomain) {
			return true
		}
	}

	return false
}

func truncateToolResult(result string, maxSize int) string {
	if len(result) <= maxSize {
		return result
	}

	// Keep first 40% and last 40% of the result
	keepStart := maxSize * 2 / 5
	keepEnd := maxSize * 2 / 5

	truncated := result[:keepStart] +
		fmt.Sprintf("\n\n[... %d characters truncated ...]\n\n", len(result)-maxSize) +
		result[len(result)-keepEnd:]

	slog.Info("Tool result truncated", "original_size", len(result), "truncated_size", maxSize, "truncated_chars", len(result)-maxSize)

	return truncated
}

func summarizeToolCall(toolName string, args map[string]any) string {
	switch toolName {
	case "run_command":
		command, ok := args["command"].(string)
		if !ok || strings.TrimSpace(command) == "" {
			return toolName
		}
		return fmt.Sprintf("%s: %s", toolName, strings.TrimSpace(command))

	case "read_file", "tree":
		path, _ := args["path"].(string)
		path = strings.TrimSpace(path)
		return fmt.Sprintf("%s: %s", toolName, path)

	case "read_multiple_files", "search_files":
		paths, ok := args["paths"].([]any)
		if !ok || len(paths) == 0 {
			return toolName
		}
		pathList := make([]string, 0, len(paths))
		for _, p := range paths {
			if path, ok := p.(string); ok {
				pathList = append(pathList, strings.TrimSpace(path))
			}
		}
		return fmt.Sprintf("%s: %s", toolName, strings.Join(pathList, ", "))
	case "search_within_files":
		substring, _ := args["substring"].(string)
		substring = strings.TrimSpace(substring)
		path, _ := args["path"].(string)
		path = strings.TrimSpace(path)
		return fmt.Sprintf("%s: search for '%s' in %s", toolName, substring, path)

	case "fetch":
		urlStr, _ := args["url"].(string)
		urlStr = strings.TrimSpace(urlStr)
		return fmt.Sprintf("%s: %s", toolName, urlStr)
	default:
		return toolName
	}
}

func hasTool(tools []*mcp.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// containsToolCallPatterns checks if the content contains patterns that suggest
// the LLM tried to make a tool call in text format instead of using the proper tool calling API
func containsToolCallPatterns(content string) bool {
	content = strings.ToLower(content)

	// Check for common tool call patterns
	patterns := []string{
		"<tool_call>",
		"<toolcall>",
		"<function_call>",
		"<functioncall>",
		"tool_call:",
		"toolcall:",
		"function_call:",
		"functioncall:",
	}

	for _, pattern := range patterns {
		if strings.Contains(content, pattern) {
			return true
		}
	}

	return false
}

func envIntOrDefault(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(v)
	if err != nil || parsed <= 0 {
		return fallback
	}

	return parsed
}

func envStringOrDefault(key string, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func envBoolOrDefault(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("Invalid boolean value, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}

	return parsed
}

func envOrFail(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fatalf("missing required environment variable: %s", key)
	}
	return v
}

func fatalf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// gitlabEnvConfig holds the GitLab connection details read from the environment.
type gitlabEnvConfig struct {
	url       string
	token     string
	projectID string
	mrIID     int64
}

// gitlabEnvFromEnv reads the GitLab configuration from the environment. It
// returns ok=false when any of the required variables is missing or invalid,
// because the GitLab integration is optional.
func gitlabEnvFromEnv() (gitlabEnvConfig, bool) {
	cfg := gitlabEnvConfig{
		url:       os.Getenv("GITLAB_URL"),
		token:     os.Getenv("GITLAB_TOKEN"),
		projectID: os.Getenv("GITLAB_PROJECT_ID"),
	}

	mrIID := os.Getenv("GITLAB_MR_IID")
	if cfg.url == "" || cfg.token == "" || cfg.projectID == "" || mrIID == "" {
		return cfg, false
	}

	parsed, err := strconv.ParseInt(mrIID, 10, 64)
	if err != nil {
		slog.Warn("Ignoring invalid GITLAB_MR_IID", "value", mrIID, "error", err)
		return cfg, false
	}
	cfg.mrIID = parsed

	return cfg, true
}

// newClient returns an authenticated GitLab API client.
func (c gitlabEnvConfig) newClient() (*gitlab.Client, error) {
	return gitlab.NewClient(c.token, gitlab.WithBaseURL(c.url))
}

// loadMergeRequestHistory fetches the existing merge request metadata and
// comments so the agent can take previous reviews into account. It never aborts
// the run: when the history is disabled or unavailable an empty string is
// returned.
func loadMergeRequestHistory(ctx context.Context) string {
	if !envBoolOrDefault("INCLUDE_MR_HISTORY", true) {
		slog.Info("Merge request history injection disabled")
		return ""
	}

	cfg, ok := gitlabEnvFromEnv()
	if !ok {
		slog.Info("Merge request history injection skipped: missing or invalid GitLab environment variables")
		return ""
	}

	maxChars := envIntOrDefault("MR_HISTORY_MAX_CHARS", 20000)
	includeSystemNotes := envBoolOrDefault("MR_HISTORY_INCLUDE_SYSTEM_NOTES", false)

	history, err := fetchMergeRequestHistory(ctx, cfg, maxChars, includeSystemNotes)
	if err != nil {
		slog.Warn("Failed to fetch merge request history, continuing without it", "error", err)
		return ""
	}

	slog.Info("Injected merge request history",
		"project", cfg.projectID,
		"mr_iid", cfg.mrIID,
		"chars", len(history),
	)

	return history
}

// fetchMergeRequestHistory collects the merge request metadata and the previous
// comments. The comments are requested newest first so the size limit drops the
// oldest comments first. Pagination is not used on purpose: merge request
// histories are expected to be short.
func fetchMergeRequestHistory(ctx context.Context, cfg gitlabEnvConfig, maxChars int, includeSystemNotes bool) (string, error) {
	client, err := cfg.newClient()
	if err != nil {
		return "", fmt.Errorf("failed to create GitLab client: %w", err)
	}

	// The metadata is best effort: the comments alone are still useful.
	mr, _, err := client.MergeRequests.GetMergeRequest(cfg.projectID, cfg.mrIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		slog.Warn("Failed to fetch merge request metadata", "error", err)
		mr = nil
	}

	orderBy := "created_at"
	sortOrder := "desc"
	notes, _, err := client.Notes.ListMergeRequestNotes(cfg.projectID, cfg.mrIID, &gitlab.ListMergeRequestNotesOptions{
		ListOptions: gitlab.ListOptions{
			PerPage: mrHistoryNotesPerPage,
			Page:    1,
		},
		OrderBy: &orderBy,
		Sort:    &sortOrder,
	}, gitlab.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("failed to list merge request notes: %w", err)
	}

	return formatMergeRequestHistory(mr, notes, maxChars, includeSystemNotes), nil
}

func postGitLabMRComment(ctx context.Context, comment string) error {
	cfg, ok := gitlabEnvFromEnv()
	if !ok {
		slog.Info("GitLab MR comment posting skipped: missing or invalid GitLab environment variables")
		return nil
	}

	if len(comment) < 100 {
		slog.Info("GitLab MR comment posting skipped: comment too short", "length", len(comment))
		return nil
	}

	slog.Info("Posting comment to GitLab MR", "project", cfg.projectID, "mr_iid", cfg.mrIID)

	client, err := cfg.newClient()
	if err != nil {
		return fmt.Errorf("failed to create GitLab client: %w", err)
	}

	// Check if the merge request is already merged
	mr, _, err := client.MergeRequests.GetMergeRequest(
		cfg.projectID,
		cfg.mrIID,
		nil,
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("failed to get merge request: %w", err)
	}

	if mr.MergedAt != nil {
		slog.Info("GitLab MR comment posting skipped: MR already merged", "project", cfg.projectID, "mr_iid", cfg.mrIID)
		return nil
	}

	// List all merge request notes to find existing agent comment
	notes, _, err := client.Notes.ListMergeRequestNotes(
		cfg.projectID,
		cfg.mrIID,
		&gitlab.ListMergeRequestNotesOptions{},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("failed to list MR notes: %w", err)
	}

	// Search for existing agent comment
	var existingNoteID int64
	for _, note := range notes {
		if strings.Contains(note.Body, agentCommentMarker) {
			existingNoteID = note.ID
			slog.Info("Found existing agent comment", "note_id", note.ID)
			break
		}
	}

	// Prepare the comment body with the marker
	commentBody := fmt.Sprintf("%s\n\n%s", agentCommentMarker, comment)

	if existingNoteID > 0 {
		// Update existing comment
		_, _, err := client.Notes.UpdateMergeRequestNote(
			cfg.projectID,
			cfg.mrIID,
			existingNoteID,
			&gitlab.UpdateMergeRequestNoteOptions{
				Body: new(commentBody),
			},
			gitlab.WithContext(ctx),
		)
		if err != nil {
			return fmt.Errorf("failed to update MR note: %w", err)
		}
		slog.Info("Successfully updated comment to GitLab MR", "note_id", existingNoteID)
	} else {
		// Create new comment
		note, _, err := client.Notes.CreateMergeRequestNote(
			cfg.projectID,
			cfg.mrIID,
			&gitlab.CreateMergeRequestNoteOptions{
				Body: new(commentBody),
			},
			gitlab.WithContext(ctx),
		)
		if err != nil {
			return fmt.Errorf("failed to create MR note: %w", err)
		}
		slog.Info("Successfully posted comment to GitLab MR", "note_id", note.ID)
	}

	return nil
}
