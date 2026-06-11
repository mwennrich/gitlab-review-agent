package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
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
	systemPrompt := os.Getenv("SYSTEM_PROMPT")
	if systemPrompt == "" {
		systemPrompt = fmt.Sprintf("You are an automated code review system. The repo is in %s. Use the 'filesystem' tools to inspect files in the %s directory, and the 'run_command' shell tool for read-only commands such as git, rg, ls, cat, sed, and find.", repoPath, repoPath)
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

	maxSteps := envIntOrDefault("MAX_STEPS", 200)

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
	fsClient, err := client.NewStdioMCPClient("npx", []string{}, "-y", "@modelcontextprotocol/server-filesystem", repoPath)
	if err != nil {
		fatalf("failed to start filesystem client: %v", err)
	}
	defer func() {
		if err := fsClient.Close(); err != nil {
			slog.Error("failed to close filesystem client", "error", err)
		}
	}()

	shellClient, err := newShellMCPClient()
	if err != nil {
		fatalf("failed to start shell client: %v", err)
	}
	defer func() {
		if err := shellClient.Close(); err != nil {
			slog.Error("failed to close shell client", "error", err)
		}
	}()

	// initialize MCP clients and fetch tool lists
	slog.Info("Connecting to MCP servers")
	slog.Debug(" ... initializing filesystem client")
	fsInit := mcp.InitializeRequest{}
	fsInit.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	fsInit.Params.ClientInfo = mcp.Implementation{Name: "go-review-agent", Version: "1.0.0"}
	if _, err := fsClient.Initialize(ctx, fsInit); err != nil {
		fatalf("filesystem initialization failed: %v", err)
	}
	slog.Debug(" ... initializing shell client")
	shellInit := mcp.InitializeRequest{}
	shellInit.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	shellInit.Params.ClientInfo = mcp.Implementation{Name: "go-review-agent", Version: "1.0.0"}
	if _, err := shellClient.Initialize(ctx, shellInit); err != nil {
		fatalf("shell initialization failed: %v", err)
	}

	slog.Debug("Fetching tool lists from MCP servers")
	fsToolsRes, err := fsClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		fatalf("failed to load filesystem tools: %v", err)
	}
	shellToolsRes, err := shellClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		fatalf("failed to load shell tools: %v", err)
	}

	// build OpenAI tool definitions from the MCP tool lists
	allMCPTools := append(fsToolsRes.Tools, shellToolsRes.Tools...)
	// allMCPTools = append(allMCPTools, thinkToolsRes.Tools...)
	oaiTools := buildOpenAITools(allMCPTools)

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(taskPrompt),
	}

	startedAt := time.Now()
	stepsExecuted := 0
	toolCallsExecuted := 0
	stopReason := "max_steps_reached"
	finalAnswer := ""

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
			stopReason = "final_response"
			finalAnswer = msg.Content
			break
		}

		prettyLogReasoning(resp)

		toolCallsExecuted += len(msg.ToolCalls)
		for _, tc := range msg.ToolCalls {
			resultText := executeToolCall(ctx, tc, fsToolsRes.Tools, shellToolsRes.Tools, fsClient, shellClient)
			slog.Debug("Tool call result", "tool", tc.Function.Name, "result", resultText)
			messages = append(messages, openai.ToolMessage(resultText, tc.ID))
		}
	}

	switch stopReason {
	case "final_response":
		slog.Info("Final model response")
		fmt.Println(finalAnswer)
		// Post the final answer as a comment to GitLab MR if configured
		if err := postGitLabMRComment(ctx, finalAnswer); err != nil {
			slog.Error("Failed to post GitLab MR comment", "error", err)
		}
	case "no_model_response":
		slog.Info("No response received from the model")
	default:
		slog.Info("Reached max steps without a final answer. Increase MAX_STEPS if needed", "max_steps", maxSteps)
	}

	duration := time.Since(startedAt).Round(time.Millisecond)
	slog.Info("Run summary", "stop_reason", stopReason, "steps_executed", stepsExecuted, "tool_calls_executed", toolCallsExecuted, "duration", duration)
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

func buildOpenAITools(tools []mcp.Tool) []openai.ChatCompletionToolUnionParam {
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

func executeToolCall(ctx context.Context, tc openai.ChatCompletionMessageToolCallUnion, fsTools []mcp.Tool, shellTools []mcp.Tool, fsClient *client.Client, shellClient *client.Client) string {
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

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = parsed

	var (
		callResult *mcp.CallToolResult
		err        error
	)

	if hasTool(fsTools, toolName) {
		callResult, err = fsClient.CallTool(ctx, req)
	} else if hasTool(shellTools, toolName) {
		callResult, err = shellClient.CallTool(ctx, req)
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

	return string(payload)
}

func summarizeToolCall(toolName string, args map[string]any) string {
	switch toolName {
	case "run_command":
		command, ok := args["command"].(string)
		if !ok || strings.TrimSpace(command) == "" {
			return toolName
		}

		parts := []string{strings.TrimSpace(command)}
		if rawArgs, ok := args["args"].([]any); ok {
			for _, rawArg := range rawArgs {
				if arg, ok := rawArg.(string); ok {
					parts = append(parts, strings.TrimSpace(arg))
				}
			}
		}
		return fmt.Sprintf("%s: %s", toolName, strings.Join(parts, " "))

	case "read_text_file":
		path, _ := args["path"].(string)
		path = strings.TrimSpace(path)
		return fmt.Sprintf("%s: %s", toolName, path)

	case "read_multiple_files":
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

	default:
		return toolName
	}
}

func hasTool(tools []mcp.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
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

func newShellMCPClient() (*client.Client, error) {
	command, args := shellMCPCommand()
	return client.NewStdioMCPClient(command, []string{}, args...)
}

func shellMCPCommand() (string, []string) {
	if _, err := os.Stat("/app/shellmcp"); err == nil {
		return "/app/shellmcp", nil
	}
	return "go", []string{"run", "./cmd/shellmcp"}
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
