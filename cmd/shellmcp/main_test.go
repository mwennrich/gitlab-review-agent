package main

import (
	"context"
	"os"
	"testing"
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
