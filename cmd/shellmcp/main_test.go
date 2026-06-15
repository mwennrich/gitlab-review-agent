package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestParseCommandString(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []command
		wantErr bool
	}{
		{
			name:  "single command",
			input: "ls -la",
			want: []command{
				{name: "ls", args: []string{"-la"}},
			},
			wantErr: false,
		},
		{
			name:  "pipeline with two commands",
			input: "ls | grep test",
			want: []command{
				{name: "ls", args: []string{}},
				{name: "grep", args: []string{"test"}},
			},
			wantErr: false,
		},
		{
			name:  "pipeline with three commands",
			input: "ls | grep test | head -5",
			want: []command{
				{name: "ls", args: []string{}},
				{name: "grep", args: []string{"test"}},
				{name: "head", args: []string{"-5"}},
			},
			wantErr: false,
		},
		{
			name:    "empty command",
			input:   "",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "empty command in pipeline",
			input:   "ls | | grep test",
			want:    nil,
			wantErr: true,
		},
		{
			name:  "pipeline with spaces around pipes",
			input: "ls  |  grep test  |  head -5",
			want: []command{
				{name: "ls", args: []string{}},
				{name: "grep", args: []string{"test"}},
				{name: "head", args: []string{"-5"}},
			},
			wantErr: false,
		},
		{
			name:  "pipeline with spaces in quoted arguments",
			input: "ls  |  grep 'test 123'",
			want: []command{
				{name: "ls", args: []string{}},
				{name: "grep", args: []string{"test 123"}},
			},
			wantErr: false,
		},
		{
			name:  "pipeline with escaped spaces in arguments",
			input: "ls  |  grep test\\ 123",
			want: []command{
				{name: "ls", args: []string{}},
				{name: "grep", args: []string{"test 123"}},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCommandString(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseCommandString() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("parseCommandString() got %d commands, want %d", len(got), len(tt.want))
					return
				}
				for i := range got {
					if got[i].name != tt.want[i].name {
						t.Errorf("parseCommandString() command %d name = %v, want %v", i, got[i].name, tt.want[i].name)
					}
					if len(got[i].args) != len(tt.want[i].args) {
						t.Errorf("parseCommandString() command %d args length = %v, want %v", i, len(got[i].args), len(tt.want[i].args))
					}
				}
			}
		})
	}
}

func TestGetCommandTimeout(t *testing.T) {
	// Save original value
	originalTimeout := os.Getenv("SHELL_COMMAND_TIMEOUT")
	defer os.Setenv("SHELL_COMMAND_TIMEOUT", originalTimeout)

	tests := []struct {
		name          string
		envValue      string
		expected      time.Duration
		description   string
	}{
		{
			name:        "default timeout when env not set",
			envValue:    "",
			expected:    30 * time.Second,
			description: "Should return 30 seconds when SHELL_COMMAND_TIMEOUT is not set",
		},
		{
			name:        "custom timeout",
			envValue:    "60",
			expected:    60 * time.Second,
			description: "Should return 60 seconds when SHELL_COMMAND_TIMEOUT=60",
		},
		{
			name:        "small timeout",
			envValue:    "5",
			expected:    5 * time.Second,
			description: "Should return 5 seconds when SHELL_COMMAND_TIMEOUT=5",
		},
		{
			name:        "invalid value - non-numeric",
			envValue:    "invalid",
			expected:    30 * time.Second,
			description: "Should return default 30 seconds for invalid value",
		},
		{
			name:        "invalid value - negative",
			envValue:    "-10",
			expected:    30 * time.Second,
			description: "Should return default 30 seconds for negative value",
		},
		{
			name:        "invalid value - zero",
			envValue:    "0",
			expected:    30 * time.Second,
			description: "Should return default 30 seconds for zero value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variable
			if tt.envValue == "" {
				os.Unsetenv("SHELL_COMMAND_TIMEOUT")
			} else {
				os.Setenv("SHELL_COMMAND_TIMEOUT", tt.envValue)
			}

			// Get timeout
			got := getCommandTimeout()

			// Check result
			if got != tt.expected {
				t.Errorf("%s: getCommandTimeout() = %v, want %v", tt.description, got, tt.expected)
			}
		})
	}
}

func TestTimeoutBehavior(t *testing.T) {
	// Save original value
	originalTimeout := os.Getenv("SHELL_COMMAND_TIMEOUT")
	defer os.Setenv("SHELL_COMMAND_TIMEOUT", originalTimeout)

	// Set a very short timeout for testing
	os.Setenv("SHELL_COMMAND_TIMEOUT", "1")

	// Create context with timeout (simulating what handleRunCommand does)
	timeout := getCommandTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	dir, _ := os.Getwd()

	// Test with a command that will timeout
	// Using sleep with a duration longer than the timeout
	pipeline := []command{
		{name: "sleep", args: []string{"5"}},
	}

	start := time.Now()
	_, err := runPipeline(ctx, dir, pipeline)
	duration := time.Since(start)

	// Command should have failed due to timeout
	if err == nil {
		t.Error("Expected timeout error, but command succeeded")
	}

	// Should have taken approximately 1 second (the timeout), not 5 seconds
	// Allow some margin for execution overhead
	if duration > 2*time.Second {
		t.Errorf("Command took too long: %v, expected ~1s (timeout)", duration)
	}

	// Should have taken at least close to the timeout
	if duration < 500*time.Millisecond {
		t.Errorf("Command finished too quickly: %v, expected ~1s (timeout)", duration)
	}
}

func TestRunPipeline(t *testing.T) {
	ctx := context.Background()
	dir, _ := os.Getwd()

	tests := []struct {
		name    string
		pipeline []command
		wantErr bool
	}{
		{
			name: "single command",
			pipeline: []command{
				{name: "pwd", args: []string{}},
			},
			wantErr: false,
		},
		{
			name: "pipeline with two commands",
			pipeline: []command{
				{name: "echo", args: []string{"hello world"}},
				{name: "grep", args: []string{"hello"}},
			},
			wantErr: false,
		},
		{
			name: "pipeline with three commands",
			pipeline: []command{
				{name: "echo", args: []string{"hello\nworld\ntest"}},
				{name: "grep", args: []string{"hello"}},
				{name: "wc", args: []string{"-l"}},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := runPipeline(ctx, dir, tt.pipeline)
			if (err != nil) != tt.wantErr {
				t.Errorf("runPipeline() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && output == "" {
				t.Errorf("runPipeline() expected output, got empty string")
			}
		})
	}
}
