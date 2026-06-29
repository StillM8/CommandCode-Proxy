# CommandCode Proxy

OpenAI-compatible API proxy for [CommandCode CLI](https://github.com/nicholasgriffintn/command-code). Turns your local `cmd` tool into a remote API that any OpenAI-compatible client can use.

## Features

- **OpenAI-Compatible API** — Works with any client that supports `/v1/chat/completions`
- **34+ Models** — Automatically fetches available models from your subscription
- **Streaming & Non-Streaming** — Real-time SSE streaming responses
- **Native Sessions** — Uses cmd's built-in `--resume` for conversation persistence
- **Plan Mode** — Read-only mode via `--plan`
- **Permission Control** — Expose `standard|plan|auto-accept` modes
- **Workspace Context** — Add directories via `--add-dir`
- **Session Forking** — Fork sessions via `--fork-session`
- **Retry with Backoff** — Automatic retry on cmd failures
- **Connection Pooling** — Limits concurrent cmd processes
- **Request Timeout** — Configurable per-request timeout
- **Graceful Shutdown** — Clean stop on SIGINT/SIGTERM
- **CORS** — Full browser support

## Quick Start

```bash
# 1. Authenticate
cmd auth login

# 2. Build
go build -o commandcode-proxy .

# 3. Run
export PROXY_API_KEY="your-secret-key"
./commandcode-proxy

# 4. Use
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PROXY_API_KEY` | (required) | API key for client authentication |
| `PORT` | `8080` | Server port |
| `CMD_PATH` | `cmd` | Path to cmd binary |
| `MAX_RETRIES` | `3` | Max retry attempts on failure |
| `RETRY_DELAY_MS` | `1000` | Base delay between retries (ms) |
| `MAX_CONCURRENT` | `4` | Max concurrent cmd processes |
| `REQUEST_TIMEOUT_SEC` | `300` | Max time per request (seconds) |
| `MAX_REQUEST_SIZE_MB` | `10` | Max request body size (MB) |
| `MAX_TURNS` | `10` | Max conversation turns in -p mode |

## Available Models

Models are fetched automatically from `cmd --list-models`. Use any model ID directly:

**Open Source:** `deepseek/deepseek-v4-pro`, `moonshotai/Kimi-K2.7-Code`, `zai-org/GLM-5.2`, `Qwen/Qwen3.7-Max`...

**Anthropic:** `claude-sonnet-4-6`, `claude-fable-5`, `claude-opus-4-8`, `claude-haiku-4-5`

**OpenAI:** `gpt-5.5`, `gpt-5.4`, `gpt-5.3-codex`, `gpt-5.4-mini`

**Google:** `google/gemini-3.5-flash`, `google/gemini-3.1-flash-lite`

## API Endpoints

### Chat Completions
```
POST /v1/chat/completions
```

**Standard fields:**
```json
{
  "model": "claude-sonnet-4-6",
  "messages": [{"role": "user", "content": "Hello!"}],
  "stream": true
}
```

**CommandCode extensions:**
```json
{
  "model": "claude-sonnet-4-6",
  "messages": [{"role": "user", "content": "Analyze this code"}],
  "stream": false,
  "plan": true,
  "add_dir": "/my/project",
  "permission_mode": "auto-accept"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `continue` | bool | Resume last session (no ID needed) |
| `plan` | bool | Plan mode — reads code but doesn't edit |
| `permission_mode` | string | `standard`, `plan`, or `auto-accept` |
| `add_dir` | string | Add directory to workspace context |
| `fork_session` | bool | Fork the session (with `--resume`) |

### Health
```
GET /health
```

### List Models
```
GET /v1/models
```

## Conversation Resume

Sessions use cmd's native persistence. Resume via header or model field:

```bash
# First request — note the X-Conversation-ID in response
curl -i http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer key" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"My name is Alice"}]}'

# Resume via header
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer key" \
  -H "X-Conversation-ID: my-session-id" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"What is my name?"}]}'

# Or via model field
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer key" \
  -d '{"model":"claude-sonnet-4-6:my-session-id","messages":[{"role":"user","content":"What is my name?"}]}'
```

Sessions persist across proxy restarts (stored by cmd on disk).

## Usage with SDKs

### Python
```python
from openai import OpenAI
client = OpenAI(api_key="key", base_url="http://localhost:8080/v1")
response = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "Hello!"}],
    stream=True
)
for chunk in response:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

### Node.js
```javascript
import OpenAI from 'openai';
const client = new OpenAI({ apiKey: 'key', baseURL: 'http://localhost:8080/v1' });
const stream = await client.chat.completions.create({
  model: 'claude-sonnet-4-6',
  messages: [{ role: 'user', content: 'Hello!' }],
  stream: true,
});
for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content || '');
}
```

## Docker

```bash
docker build -t commandcode-proxy .
docker run -p 8080:8080 -e PROXY_API_KEY=mykey commandcode-proxy
```

## Testing

```bash
go test -v ./...
```

## License

MIT
