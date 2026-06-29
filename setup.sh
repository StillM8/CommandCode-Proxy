#!/bin/bash
set -e

echo "CommandCode Proxy Setup"
echo "======================="

# Check cmd is installed
if ! command -v cmd &> /dev/null; then
    echo "Error: cmd not found. Install it first:"
    echo "  npm install -g command-code"
    exit 1
fi

# Check cmd is authenticated
if ! cmd status &> /dev/null 2>&1; then
    echo "cmd not authenticated. Running 'cmd auth login'..."
    cmd auth login
fi

# Check Go is installed
if ! command -v go &> /dev/null; then
    echo "Error: Go not found. Install from https://go.dev/dl/"
    exit 1
fi

# Build
echo "Building..."
go build -o commandcode-proxy .

# Generate API key
API_KEY=$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | xxd -p | tr -d '\n' | head -c 64)

# Create .env
if [ ! -f .env ]; then
    cat > .env << EOF
PROXY_API_KEY=$API_KEY
PORT=8080
CMD_PATH=cmd
MAX_RETRIES=3
MAX_CONCURRENT=4
REQUEST_TIMEOUT_SEC=300
MAX_TURNS=10
EOF
    echo ".env created with random API key"
else
    echo ".env already exists, skipping"
fi

echo ""
echo "Done! Start the proxy with:"
echo ""
echo "  source .env && export PROXY_API_KEY && ./commandcode-proxy"
echo ""
echo "Or just:"
echo "  export PROXY_API_KEY=\$(grep PROXY_API_KEY .env | cut -d= -f2) && ./commandcode-proxy"
echo ""
echo "Then point your AI client to:"
echo "  Base URL: http://localhost:8080/v1"
echo "  API Key:  $API_KEY"
echo ""
