# gitlab-review-agent (Go + openai-go + MCP)

This agent connects to two MCP servers (filesystem and a safe shell tool server), registers their tools on an OpenAI-compatible endpoint, and executes tool calls.
The shell server exposes a curated allowlist of read-only Linux commands, including git, rg, sed, ls, cat, find, head, tail, wc, and friends.

## Prerequisites

- Go 1.26+
- Node.js with npx
- An OpenAI-compatible endpoint

## Environment Variables

- OPENAI_API_KEY: API key for the endpoint
- OPENAI_BASE_URL: Base URL, for example <http://localhost:8000/v1>
- MODEL_NAME: Model name, for example gpt-4o-mini
- TASK: The concrete assignment for the agent (user prompt)
- SYSTEM_PROMPT: Optional system behavior override
- MAX_STEPS: Maximum number of agent loop iterations (default: 12)
- LOG_LEVEL: Logging severity (`info` or `debug`, default: `info`)
- REPO_PATH: Path to the repository to analyze (default: `/workspace`)
- SHELL_COMMAND_TIMEOUT: Timeout in seconds for shell commands (default: 30)
- GITLAB_URL: GitLab server URL (e.g., <https://gitlab.com>) - optional
- GITLAB_TOKEN: Personal Access Token with API scope - optional
- GITLAB_PROJECT_ID: Project ID or path (e.g., "group/project" or numeric ID) - optional
- GITLAB_MR_IID: Merge Request IID (internal ID, not the global ID) - optional

When all GitLab environment variables are set, the agent will automatically post the final review result as a comment to the specified merge request.

## How To Give The Agent A Task

Set the task at runtime via the TASK environment variable, then run the agent.

```bash
TASK="Find dead code and suggest safe removals" go run ./agent.go
```

Or put the task in a file and set the TASK_FILE environment variable:

```bash
TASK_FILE="/config/AGENTS.md" go run ./agent.go
```

At the end of each run, the agent prints a compact run summary with:

- stop reason
- executed steps
- executed tool calls
- total duration

The agent expects the repository to analyze under the path specified by `REPO_PATH` (default: `/workspace`), because the filesystem MCP server is started with that path.

## Example gitlab-ci.yml

```yaml
review-agent:
  image: ghcr.io/mwennrich/gitlab-review-agent:latest
  rules:
    - if: '$CI_PIPELINE_SOURCE == "merge_request_event"'
  script:
    - export GITLAB_MR_IID="$(printf "$CI_OPEN_MERGE_REQUESTS" | cut -f2 -d\!)"
    - git fetch --all
    - /app/agent
  variables:
    OPENAI_BASE_URL: http://localhost:8000
    MODEL_NAME: gpt-4o-mini
    REPO_PATH: "$CI_PROJECT_DIR"
    GITLAB_URL: "https://git.example.com/"
    GITLAB_TOKEN: "$GITLAB_CI_TOKEN"
    GITLAB_PROJECT_ID: "$CI_PROJECT_PATH"
    GITLAB_MR_IID: "$GITLAB_MR_IID"
    TASK: "Execute git diff origin/master and report the results."
```
