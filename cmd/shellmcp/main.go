package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

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

func main() {
	mcpServer := server.NewMCPServer(
		"mdr-shell-tools",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	mcpServer.AddTool(mcp.NewTool(
		"run_command",
		mcp.WithDescription("Run a safe read-only command in the workspace. Allowed commands are a curated allowlist of common Linux tools."),
		mcp.WithString("command", mcp.Description("Allowed command to run."), mcp.Required(), mcp.Enum(slices.Sorted(maps.Keys(allowedCommands))...)),
		mcp.WithArray("args", mcp.Description("Command arguments."), mcp.Items(map[string]any{"type": "string"})),
		mcp.WithString("cwd", mcp.Description("Optional working directory relative to the workspace root."), mcp.DefaultString(".")),
	), handleRunCommand)

	if err := server.ServeStdio(mcpServer); err != nil {
		log.Fatalf("shell MCP server failed: %v", err)
	}
}

func handleRunCommand(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	commandName := request.Params.Arguments["command"].(string)
	if commandName == "" {
		return mcp.NewToolResultError("missing required command"), nil
	}
	if !allowedCommands[commandName] {
		return mcp.NewToolResultError(fmt.Sprintf("command %q is not allowed", commandName)), nil
	}

	args := stringListArg(request.Params.Arguments, "args")

	cwdArg := "."
	if cwdValue, ok := request.Params.Arguments["cwd"]; ok && cwdValue != nil {
		cwdArg = cwdValue.(string)
	}

	commandDir := os.Getenv("REPO_PATH")
	if cwdArg != "" {
		commandDir = filepath.Join(commandDir, cwdArg)
	}

	output, err := runCommand(ctx, commandDir, commandName, args...)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("command failed: %v\n\n%s", err, output)), nil
	}
	return mcp.NewToolResultText(output), nil
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

func stringListArg(args map[string]any, key string) []string {
	items, ok := args[key].([]any)
	if !ok {
		return nil
	}

	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			if text = strings.TrimSpace(text); text != "" {
				result = append(result, text)
			}
		}
	}

	return result
}
