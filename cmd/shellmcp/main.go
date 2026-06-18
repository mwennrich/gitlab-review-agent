package main

import (
	"bytes"
	"context"
	"fmt"
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
	name string
	args []string
}

var allowedCommands = map[string]bool{
	"awk":      true,
	"basename": true,
	"cat":      true,
	"dirname":  true,
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
	Command string `json:"command" jsonschema:"Command to run. Can include pipes (|) to chain multiple commands."`
	CWD     string `json:"cwd" jsonschema:"Optional working directory relative to the workspace root."`
}

func main() {
	impl := &mcp.Implementation{Name: "shell-tools", Version: "0.2.1"}

	mcpServer := mcp.NewServer(impl, nil)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "run_command",
		Description: "Run a safe read-only command in the workspace. Allowed commands are a curated allowlist of common Linux tools. Supports pipes (|) to chain commands.",
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

func runCommand(ctx context.Context, dir string, command string, args ...string) (string, error) {
	if command == "git" {
		args = append([]string{"-c", "safe.directory=" + os.Getenv("REPO_PATH")}, args...)
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// parseCommandString parses a command string into a pipeline of commands
func parseCommandString(cmdString string) ([]command, error) {
	// Split by pipe character
	parts := strings.Split(cmdString, "|")
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

		cmd := command{
			name: fields[0],
			args: fields[1:],
		}
		pipeline = append(pipeline, cmd)
	}

	return pipeline, nil
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
		return runCommand(ctx, dir, pipeline[0].name, pipeline[0].args...)
	}

	// Create commands
	cmds := make([]*exec.Cmd, len(pipeline))
	for i, cmd := range pipeline {
		if cmd.name == "git" {
			cmd.args = append([]string{"-c", "safe.directory=" + os.Getenv("REPO_PATH")}, cmd.args...)
		}
		cmds[i] = exec.CommandContext(ctx, cmd.name, cmd.args...)
		cmds[i].Dir = dir
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
	cmds[len(cmds)-1].Stdout = &output
	cmds[len(cmds)-1].Stderr = &output

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
