package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// command represents a single command in a pipeline
type command struct {
	name           string
	args           []string
	suppressStderr bool
}

const suppressStderrToken = "2>/dev/null"

var allowedCommands = map[string]bool{
	"awk":      true,
	"basename": true,
	"cat":      true,
	"dirname":  true,
	"echo":     true,
	"find":     true,
	"git":      true,
	"grep":     true,
	"head":     true,
	"ls":       true,
	"pwd":      true,
	"rg":       true,
	"sed":      true,
	"sort":     true,
	"tail":     true,
	"uniq":     true,
	"wc":       true,
}

type RunCommandInput struct {
	Command string `json:"command" jsonschema:"Command to run. Supports only pipelines using |. Does not support shell control operators such as ;, &&, ||, command substitution, or general redirection. Special case supported: token 2>/dev/null to suppress stderr for that command."`
	CWD     string `json:"cwd" jsonschema:"Optional working directory."`
}

func main() {
	impl := &mcp.Implementation{Name: "shell-tools", Version: "0.2.1"}

	mcpServer := mcp.NewServer(impl, nil)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "run_command",
		Description: "Run a safe read-only command in the workspace. Allowed commands are from a curated allowlist of common Linux tools. Supports only pipelines using |. Does not support shell control operators like ;, &&, ||, command substitution, or general redirection. Special case supported: token 2>/dev/null to suppress stderr for that command.",
	}, handleRunCommand)

	if err := mcpServer.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("shell MCP server failed: %v", err)
	}
}

func getCommandTimeout() time.Duration {
	timeoutStr := os.Getenv("SHELL_COMMAND_TIMEOUT")
	if timeoutStr == "" {
		return 30 * time.Second // Default timeout
	}

	timeoutSeconds, err := strconv.Atoi(timeoutStr)
	if err != nil || timeoutSeconds <= 0 {
		log.Printf("Invalid SHELL_COMMAND_TIMEOUT value '%s', using default 30s", timeoutStr)
		return 30 * time.Second
	}

	return time.Duration(timeoutSeconds) * time.Second
}

func handleRunCommand(ctx context.Context, req *mcp.CallToolRequest, in RunCommandInput) (*mcp.CallToolResult, any, error) {
	// Apply timeout to context
	timeout := getCommandTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	commandString := in.Command
	if commandString == "" {
		return nil, nil, fmt.Errorf("missing required command")
	}

	commandDir := in.CWD
	if commandDir == "" {
		commandDir = os.Getenv("REPO_PATH")
	}

	// Parse command string into pipeline
	pipeline, err := parseCommandString(commandString)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse command: %v", err)
	}

	// Validate all commands in pipeline
	for _, cmd := range pipeline {
		if !allowedCommands[cmd.name] {
			return nil, nil, fmt.Errorf("command %q is not allowed", cmd.name)
		}
	}

	// Execute pipeline
	output, err := runPipeline(ctx, commandDir, pipeline)
	if err != nil {
		return nil, nil, fmt.Errorf("command failed: %v\n\n%s", err, output)
	}
	return nil, output, nil
}

func runCommand(ctx context.Context, dir string, c command) (string, error) {
	args := c.args
	if c.name == "git" {
		args = append([]string{"-c", "safe.directory=" + os.Getenv("REPO_PATH")}, args...)
	}

	cmd := exec.CommandContext(ctx, c.name, args...)
	cmd.Dir = dir

	if c.suppressStderr {
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = io.Discard
		err := cmd.Run()
		return strings.TrimSpace(stdout.String()), err
	}

	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// parseCommandString parses a command string into a pipeline of commands
func parseCommandString(cmdString string) ([]command, error) {
	parts, err := splitPipelineParts(cmdString)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	pipeline := make([]command, 0, len(parts))
	for _, part := range parts {
		// Trim whitespace
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty command in pipeline")
		}

		// Split into command and arguments, respecting quotes
		fields, err := parseFields(part)
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf("invalid command format")
		}

		args, suppressStderr := normalizeSpecialArgs(fields[1:])

		cmd := command{
			name:           fields[0],
			args:           args,
			suppressStderr: suppressStderr,
		}
		pipeline = append(pipeline, cmd)
	}

	return pipeline, nil
}

// normalizeSpecialArgs handles explicit parser-level special cases without enabling full shell semantics.
func normalizeSpecialArgs(args []string) ([]string, bool) {
	normalized := make([]string, 0, len(args))
	suppressStderr := false

	for _, arg := range args {
		if arg == suppressStderrToken {
			suppressStderr = true
			continue
		}
		normalized = append(normalized, arg)
	}

	return normalized, suppressStderr
}

// splitPipelineParts splits a command string at unquoted, unescaped pipe characters.
func splitPipelineParts(cmdString string) ([]string, error) {
	var parts []string
	var currentPart strings.Builder
	var inSingleQuote, inDoubleQuote bool
	var escapeNext bool

	for i := 0; i < len(cmdString); i++ {
		char := cmdString[i]

		switch {
		case escapeNext:
			currentPart.WriteByte(char)
			escapeNext = false
		case char == '\\' && !inSingleQuote && !inDoubleQuote:
			currentPart.WriteByte(char)
			escapeNext = true
		case char == '\'' && !inDoubleQuote:
			inSingleQuote = !inSingleQuote
			currentPart.WriteByte(char)
		case char == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
			currentPart.WriteByte(char)
		case char == '|' && !inSingleQuote && !inDoubleQuote:
			parts = append(parts, currentPart.String())
			currentPart.Reset()
		default:
			currentPart.WriteByte(char)
		}
	}

	if inSingleQuote || inDoubleQuote {
		return nil, fmt.Errorf("unclosed quote in command")
	}
	if escapeNext {
		return nil, fmt.Errorf("incomplete escape sequence at end of command")
	}

	parts = append(parts, currentPart.String())
	return parts, nil
}

// parseFields splits a string into fields, respecting single and double quotes and backslash escaping
func parseFields(s string) ([]string, error) {
	var fields []string
	var currentField strings.Builder
	var inSingleQuote, inDoubleQuote bool
	var escapeNext bool

	for i := 0; i < len(s); i++ {
		char := s[i]

		switch {
		case escapeNext:
			// Add the escaped character literally
			currentField.WriteByte(char)
			escapeNext = false
		case char == '\\' && !inSingleQuote && !inDoubleQuote:
			// Start escape sequence (only outside quotes)
			escapeNext = true
		case char == '\'' && !inDoubleQuote:
			inSingleQuote = !inSingleQuote
		case char == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
		case char == ' ' && !inSingleQuote && !inDoubleQuote:
			if currentField.Len() > 0 {
				fields = append(fields, currentField.String())
				currentField.Reset()
			}
		default:
			currentField.WriteByte(char)
		}
	}

	// Add the last field if there is one
	if currentField.Len() > 0 {
		fields = append(fields, currentField.String())
	}

	// Check for unclosed quotes or incomplete escape
	if inSingleQuote || inDoubleQuote {
		return nil, fmt.Errorf("unclosed quote in command")
	}
	if escapeNext {
		return nil, fmt.Errorf("incomplete escape sequence at end of command")
	}

	return fields, nil
}

// runPipeline executes a pipeline of commands
func runPipeline(ctx context.Context, dir string, pipeline []command) (string, error) {
	if len(pipeline) == 0 {
		return "", fmt.Errorf("empty pipeline")
	}

	// Single command - use simple execution
	if len(pipeline) == 1 {
		return runCommand(ctx, dir, pipeline[0])
	}

	// Create commands
	cmds := make([]*exec.Cmd, len(pipeline))
	for i, c := range pipeline {
		args := c.args
		if c.name == "git" {
			args = append([]string{"-c", "safe.directory=" + os.Getenv("REPO_PATH")}, args...)
		}
		cmds[i] = exec.CommandContext(ctx, c.name, args...)
		cmds[i].Dir = dir
		if c.suppressStderr {
			cmds[i].Stderr = io.Discard
		}
	}

	// Create pipes between commands
	for i := 0; i < len(cmds)-1; i++ {
		stdout, err := cmds[i].StdoutPipe()
		if err != nil {
			return "", fmt.Errorf("failed to create stdout pipe for command %d: %w", i, err)
		}
		cmds[i+1].Stdin = stdout
	}

	// Capture output from last command
	var output bytes.Buffer
	lastCmd := cmds[len(cmds)-1]
	lastPipelineCmd := pipeline[len(pipeline)-1]
	lastCmd.Stdout = &output
	if !lastPipelineCmd.suppressStderr {
		lastCmd.Stderr = &output
	}

	// Start all commands
	for i, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("failed to start command %d: %w", i, err)
		}
	}

	// Wait for all commands to complete
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			return output.String(), fmt.Errorf("command %d failed: %w", i, err)
		}
	}

	return strings.TrimSpace(output.String()), nil
}
