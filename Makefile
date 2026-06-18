APP_NAME := gitlab-review-agent
REPO_PATH ?= $(PWD)
TASK_FILE ?= $(PWD)/AGENTS.md
OPENAI_BASE_URL ?= http://127.0.0.1:8080
DOCKER_REGISTRY ?= docker.io/mwennrich
GIT_DIR_RELATIVE := $(shell if [ -f "$(REPO_PATH)/.git" ]; then sed -n 's/^gitdir: //p' "$(REPO_PATH)/.git"; fi)
GIT_DIR_HOST := $(shell if [ -n "$(GIT_DIR_RELATIVE)" ]; then realpath -m "$(REPO_PATH)/$(GIT_DIR_RELATIVE)"; fi)
GIT_DIR_CONTAINER := $(shell if [ -n "$(GIT_DIR_RELATIVE)" ]; then realpath -m "/workspace/$(GIT_DIR_RELATIVE)"; fi)
DOCKER_TAG := $(or ${GIT_TAG_NAME}, latest)

.PHONY: tidy build run docker-build docker-run

tidy:
	go mod tidy

build:
	go build ./...

run:
	go run ./agent.go

docker-build:
	@docker build -t $(DOCKER_REGISTRY)/$(APP_NAME):$(DOCKER_TAG) .

docker-run: docker-build
	@docker run --rm \
		--network host \
		-e OPENAI_API_KEY="$$OPENAI_API_KEY" \
		-e OPENAI_BASE_URL="$(OPENAI_BASE_URL)" \
		-e MODEL_NAME="$$MODEL_NAME" \
		-e TASK_FILE="/config/AGENTS.md" \
		-e TASK="$$TASK" \
		-e SYSTEM_PROMPT="$$SYSTEM_PROMPT" \
		-e MAX_STEPS="$$MAX_STEPS" \
		-e LOG_LEVEL="$$LOG_LEVEL" \
		-e GITLAB_URL="$$GITLAB_URL" \
		-e GITLAB_TOKEN="$$GITLAB_TOKEN" \
		-e GITLAB_PROJECT_ID="$$GITLAB_PROJECT_ID" \
		-e GITLAB_MR_IID="$$GITLAB_MR_IID" \
		-e REPO_PATH="/workspace" \
		-e TARGET_BRANCH="$$TARGET_BRANCH" \
		-v "$$REPO_PATH:/workspace" \
		-v "$(PWD)/AGENTS.md:/config/AGENTS.md:ro" \
		$(DOCKER_REGISTRY)/$(APP_NAME):$(DOCKER_TAG)

docker-push: docker-build
	@docker push $(DOCKER_REGISTRY)/$(APP_NAME):$(DOCKER_TAG)
