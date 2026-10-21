package main

import (
	"strings"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

func testTimestamp(minute int) *time.Time {
	ts := time.Date(2026, time.January, 2, 10, minute, 0, 0, time.UTC)
	return &ts
}

func testNote(body, author string, minute int) *gitlab.Note {
	return &gitlab.Note{
		Body:      body,
		Author:    gitlab.NoteAuthor{Username: author},
		CreatedAt: testTimestamp(minute),
	}
}

func TestFormatMergeRequestHistory(t *testing.T) {
	mr := &gitlab.MergeRequest{
		BasicMergeRequest: gitlab.BasicMergeRequest{
			Title:        "Add fco console",
			State:        "opened",
			SourceBranch: "add-fco-console",
			TargetBranch: "develop",
			Author:       &gitlab.BasicUser{Username: "author"},
			Description:  "Adds the console role.",
			WebURL:       "https://git.example.com/group/project/-/merge_requests/7",
		},
	}

	tests := []struct {
		name               string
		mr                 *gitlab.MergeRequest
		notes              []*gitlab.Note
		maxChars           int
		includeSystemNotes bool
		wantContains       []string
		wantNotContains    []string
		wantOrder          []string
	}{
		{
			name: "renders context and comments in chronological order",
			mr:   mr,
			// the GitLab API returns the comments newest first
			notes: []*gitlab.Note{
				testNote("Newer comment", "reviewer", 5),
				testNote("Older comment", "author", 1),
			},
			maxChars: 20000,
			wantContains: []string{
				"## Merge request context",
				"- Title: Add fco console",
				"- State: opened",
				"- Source branch: add-fco-console",
				"- Target branch: develop",
				"- Author: @author",
				"- URL: https://git.example.com/group/project/-/merge_requests/7",
				"### Description",
				"Adds the console role.",
				"@reviewer 2026-01-02T10:05:00Z",
				"@author 2026-01-02T10:01:00Z",
			},
			wantOrder: []string{"### Comment 1", "Older comment", "### Comment 2", "Newer comment"},
		},
		{
			name: "flags the previous agent review and strips the marker",
			mr:   nil,
			notes: []*gitlab.Note{
				{
					Body:       "Old finding\n" + agentCommentMarker,
					Author:     gitlab.NoteAuthor{Username: "review-agent"},
					CreatedAt:  testTimestamp(3),
					Resolvable: true,
					Resolved:   true,
				},
			},
			maxChars: 20000,
			wantContains: []string{
				"@review-agent 2026-01-02T10:03:00Z",
				"resolved",
				"previous agent review, will be replaced by the new review",
				"Old finding",
			},
			wantNotContains: []string{agentCommentMarker, "unresolved", "## Merge request context"},
		},
		{
			name: "marks unresolved comments",
			mr:   nil,
			notes: []*gitlab.Note{
				{
					Body:       "Please fix this",
					Author:     gitlab.NoteAuthor{Username: "reviewer"},
					CreatedAt:  testTimestamp(4),
					Resolvable: true,
				},
			},
			maxChars:     20000,
			wantContains: []string{"unresolved"},
		},
		{
			name: "skips system notes by default",
			mr:   nil,
			notes: []*gitlab.Note{
				{Body: "added label ~bug", System: true, CreatedAt: testTimestamp(2)},
				testNote("Real comment", "reviewer", 1),
			},
			maxChars:        20000,
			wantContains:    []string{"Real comment"},
			wantNotContains: []string{"added label"},
		},
		{
			name: "includes system notes when requested",
			mr:   nil,
			notes: []*gitlab.Note{
				{Body: "added label ~bug", System: true, CreatedAt: testTimestamp(2)},
				testNote("Real comment", "reviewer", 1),
			},
			maxChars:           20000,
			includeSystemNotes: true,
			wantContains:       []string{"added label", "(system)", "Real comment"},
		},
		{
			name: "shows the file and line of comments on changed lines",
			mr:   nil,
			notes: []*gitlab.Note{
				{
					Body:      "Missing default value",
					Author:    gitlab.NoteAuthor{Username: "reviewer"},
					CreatedAt: testTimestamp(6),
					Position: &gitlab.NotePosition{
						NewPath: "control-plane/roles/fco/tasks/main.yaml",
						NewLine: 12,
					},
				},
			},
			maxChars:     20000,
			wantContains: []string{"`control-plane/roles/fco/tasks/main.yaml:12`"},
		},
		{
			name: "falls back to the old path for removed lines",
			mr:   nil,
			notes: []*gitlab.Note{
				{
					Body:      "Removed setting",
					Author:    gitlab.NoteAuthor{Username: "reviewer"},
					CreatedAt: testTimestamp(7),
					Position: &gitlab.NotePosition{
						OldPath: "control-plane/group_vars/prod/fco.yaml",
						OldLine: 25,
					},
				},
			},
			maxChars:     20000,
			wantContains: []string{"`control-plane/group_vars/prod/fco.yaml:25`"},
		},
		{
			name:     "handles merge requests without comments",
			mr:       nil,
			notes:    nil,
			maxChars: 20000,
			wantContains: []string{
				"## Previous comments (chronological)",
				"(no previous comments)",
			},
			wantNotContains: []string{"## Merge request context"},
		},
		{
			name: "ignores comments that only contain the marker",
			mr:   nil,
			notes: []*gitlab.Note{
				{
					Body:      agentCommentMarker,
					Author:    gitlab.NoteAuthor{Username: "review-agent"},
					CreatedAt: testTimestamp(8),
				},
			},
			maxChars:     20000,
			wantContains: []string{"(no previous comments)"},
		},
		{
			name: "drops the oldest comments when the size limit is exceeded",
			mr:   nil,
			notes: []*gitlab.Note{
				testNote("Newest comment", "reviewer", 5),
				testNote(strings.Repeat("x", 500), "reviewer", 1),
			},
			maxChars:     120,
			wantContains: []string{"Newest comment", "[... 1 older comment(s) omitted, MR_HISTORY_MAX_CHARS=120 ...]"},
			wantOrder:    []string{"older comment(s) omitted", "Newest comment"},
		},
		{
			name:            "truncates the newest comment instead of dropping it",
			mr:              nil,
			notes:           []*gitlab.Note{testNote(strings.Repeat("y", 300), "reviewer", 5)},
			maxChars:        40,
			wantContains:    []string{"[... truncated ...]", "### Comment 1"},
			wantNotContains: []string{"omitted"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatMergeRequestHistory(tt.mr, tt.notes, tt.maxChars, tt.includeSystemNotes)

			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("expected output to contain %q, got:\n%s", want, got)
				}
			}

			for _, unwanted := range tt.wantNotContains {
				if strings.Contains(got, unwanted) {
					t.Errorf("expected output not to contain %q, got:\n%s", unwanted, got)
				}
			}

			lastIndex := -1
			for _, want := range tt.wantOrder {
				index := strings.Index(got, want)
				if index < 0 {
					t.Fatalf("expected output to contain %q, got:\n%s", want, got)
				}
				if index < lastIndex {
					t.Errorf("expected %q after the previous entry, got:\n%s", want, got)
				}
				lastIndex = index
			}
		})
	}
}

func TestTruncateHistoryText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		maxSize int
		want    string
	}{
		{
			name:    "keeps short text unchanged",
			text:    "short comment",
			maxSize: 100,
			want:    "short comment",
		},
		{
			name:    "keeps the beginning of long text",
			text:    strings.Repeat("a", 100),
			maxSize: 30,
			want:    strings.Repeat("a", 30-len(" [... truncated ...]")) + " [... truncated ...]",
		},
		{
			name:    "handles limits smaller than the notice",
			text:    strings.Repeat("b", 50),
			maxSize: 5,
			want:    "bbbbb",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateHistoryText(tt.text, tt.maxSize); got != tt.want {
				t.Errorf("truncateHistoryText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitlabEnvFromEnv(t *testing.T) {
	setEnv := func(url, token, projectID, mrIID string) {
		t.Setenv("GITLAB_URL", url)
		t.Setenv("GITLAB_TOKEN", token)
		t.Setenv("GITLAB_PROJECT_ID", projectID)
		t.Setenv("GITLAB_MR_IID", mrIID)
	}

	t.Run("returns the parsed configuration", func(t *testing.T) {
		setEnv("https://git.example.com", "token", "group/project", "42")

		cfg, ok := gitlabEnvFromEnv()
		if !ok {
			t.Fatal("expected configuration to be complete")
		}
		if cfg.projectID != "group/project" || cfg.mrIID != 42 {
			t.Errorf("unexpected configuration: %+v", cfg)
		}
	})

	t.Run("reports incomplete configuration", func(t *testing.T) {
		setEnv("https://git.example.com", "", "group/project", "42")

		if _, ok := gitlabEnvFromEnv(); ok {
			t.Error("expected incomplete configuration to be rejected")
		}
	})

	t.Run("rejects a non numeric merge request iid", func(t *testing.T) {
		setEnv("https://git.example.com", "token", "group/project", "!7")

		if _, ok := gitlabEnvFromEnv(); ok {
			t.Error("expected invalid GITLAB_MR_IID to be rejected")
		}
	})
}

func TestEnvBoolOrDefault(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		fallback bool
		want     bool
	}{
		{name: "uses the fallback when unset", value: "", fallback: true, want: true},
		{name: "parses true", value: "true", fallback: false, want: true},
		{name: "parses 1", value: "1", fallback: false, want: true},
		{name: "parses false", value: "false", fallback: true, want: false},
		{name: "uses the fallback for invalid values", value: "yes", fallback: false, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_BOOL", tt.value)

			if got := envBoolOrDefault("TEST_BOOL", tt.fallback); got != tt.want {
				t.Errorf("envBoolOrDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}
