.PHONY: test docker docker-claude-code
test:
	go vet ./...
	go test -race -count=1 ./...

# linux/amd64 image for a typical VPS; tests run inside the build too
docker:
	docker buildx build --platform linux/amd64 -t claude-s40-server:0.7.0 --load .

# the same plus the Claude Code CLI, for CLAUDE_BACKEND=claude-code (subscription)
docker-claude-code:
	docker buildx build --platform linux/amd64 --target claude-code -t claude-s40-server:0.7.0-claude-code --load .
