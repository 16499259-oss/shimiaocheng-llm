.PHONY: build clean run test install

# Binary name
BINARY := llm_api_gateway

# Build settings
GO := go
GOFLAGS := CGO_ENABLED=0
LDFLAGS := -ldflags="-s -w"

# Default target
build:
	$(GOFLAGS) $(GO) build $(LDFLAGS) -o $(BINARY) .

# Build for Linux (production)
build-linux:
	GOOS=linux GOARCH=amd64 $(GOFLAGS) $(GO) build $(LDFLAGS) -o $(BINARY) .

# Run locally
run:
	$(GO) run . -config config.yaml

# Run with admin password init
init-admin:
	$(GO) run . -config config.yaml -passwd "$${ADMIN_PASSWORD:-admin123}"

# Clean build artifacts
clean:
	rm -f $(BINARY)
	rm -f llm_gateway.db

# Download dependencies
deps:
	$(GO) mod download
	$(GO) mod tidy

# Run tests
test:
	$(GO) test ./...

# Format code
fmt:
	$(GO) fmt ./...

# Vet code
vet:
	$(GO) vet ./...

# Install to /opt/llm-gateway (production)
install: build-linux
	sudo mkdir -p /opt/llm-gateway
	sudo cp $(BINARY) /opt/llm-gateway/
	sudo cp config.yaml.example /opt/llm-gateway/config.yaml
	sudo cp deploy/llm-gateway.service /etc/systemd/system/
	sudo useradd -r -s /bin/false llm 2>/dev/null || true
	sudo chown -R llm:llm /opt/llm-gateway
	sudo systemctl daemon-reload
	sudo systemctl enable llm-gateway
	@echo "Installation complete. Edit /opt/llm-gateway/config.yaml before starting."
	@echo "Then: sudo systemctl start llm-gateway"
