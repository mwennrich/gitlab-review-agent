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
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
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
			"type":       tool.InputSchema.Type,
			"properties": tool.InputSchema.Properties,
		}
		if len(tool.InputSchema.Required) > 0 {
			schema["required"] = tool.InputSchema.Required
		}
		if tool.InputSchema.Type == "" {
			schema["type"] = "object"
		}
		if tool.InputSchema.Properties == nil {
			schema["properties"] = map[string]any{}
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

	var parsed map[string]any
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
			return fmt.Sprintf("failed to parse tool arguments: %v", err)
		}
	}
	if parsed == nil {
		parsed = map[string]any{}
	}

	slog.Info("Run", "tool", summarizeToolCall(toolName, parsed))

	// Validate fetch tool URLs before execution
	if hasTool(fetchTools, toolName) {
		if urlStr, ok := parsed["url"].(string); ok {
			if !isURLAllowed(urlStr, allowedDomains) {
				slog.Warn("Fetch URL not allowed", "url", urlStr, "allowed_domains", allowedDomains)
				return fmt.Sprintf("fetch URL not allowed: %s (allowed domains: %s)", urlStr, strings.Join(allowedDomains, ", "))
			}
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
		slog.Error("Tool execution failed", "tool", toolName, "error", err)
		return fmt.Sprintf("tool execution failed: %v", err)
	}

	payload, err := json.MarshalIndent(callResult, "", "  ")
	if err != nil {
		return fmt.Sprintf("tool returned result, but marshal failed: %v", err)
	}

	result := string(payload)
	result = truncateToolResult(result, maxToolResultSize)

	return result
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

func postGitLabMRComment(ctx context.Context, comment string) error {
	// Check if GitLab configuration is present
	gitlabURL := os.Getenv("GITLAB_URL")
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	projectID := os.Getenv("GITLAB_PROJECT_ID")
	mrIID := os.Getenv("GITLAB_MR_IID")

	if gitlabURL == "" || gitlabToken == "" || projectID == "" || mrIID == "" {
		slog.Info("GitLab MR comment posting skipped: missing required environment variables")
		return nil
	}

	if len(comment) < 100 {
		slog.Info("GitLab MR comment posting skipped: comment too short", "length", len(comment))
		return nil
	}

	slog.Info("Posting comment to GitLab MR", "project", projectID, "mr_iid", mrIID)

	client, err := gitlab.NewClient(
		gitlabToken,
		gitlab.WithBaseURL(gitlabURL),
	)
	if err != nil {
		return fmt.Errorf("failed to create GitLab client: %w", err)
	}

	mrIIDInt, err := strconv.Atoi(mrIID)
	if err != nil {
		return fmt.Errorf("invalid GITLAB_MR_IID: %w", err)
	}

	// Check if the merge request is already merged
	mr, _, err := client.MergeRequests.GetMergeRequest(
		projectID,
		int64(mrIIDInt),
		nil,
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("failed to get merge request: %w", err)
	}

	if mr.MergedAt != nil {
		slog.Info("GitLab MR comment posting skipped: MR already merged", "project", projectID, "mr_iid", mrIID)
		return nil
	}

	// Define the marker to identify agent comments
	const agentCommentMarker = "<!-- AGENT_COMMENT -->"

	// List all merge request notes to find existing agent comment
	notes, _, err := client.Notes.ListMergeRequestNotes(
		projectID,
		int64(mrIIDInt),
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
			projectID,
			int64(mrIIDInt),
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
			projectID,
			int64(mrIIDInt),
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
