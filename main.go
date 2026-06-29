package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// OpenAI-compatible types

type ChatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Stream          bool      `json:"stream"`

	// CommandCode extensions (passed through to cmd)
	Continue        bool     `json:"continue,omitempty"`         // --continue: resume last session
	Plan            bool     `json:"plan,omitempty"`             // --plan: plan mode (read-only)
	PermissionMode  string   `json:"permission_mode,omitempty"`  // --permission-mode: standard|plan|auto-accept
	AddDir          string   `json:"add_dir,omitempty"`          // --add-dir: add workspace context
	ForkSession     bool     `json:"fork_session,omitempty"`     // --fork-session: fork the session
}

type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Delta   `json:"delta,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"`
}

type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type ModelInfo struct {
	ID          string `json:"id"`
	OwnedBy     string `json:"owned_by"`
	Description string `json:"description,omitempty"`
}

// Worker pool

type WorkerPool struct {
	sem chan struct{}
}

func NewWorkerPool(n int) *WorkerPool { return &WorkerPool{sem: make(chan struct{}, n)} }
func (p *WorkerPool) Acquire()        { p.sem <- struct{}{} }
func (p *WorkerPool) Release()        { <-p.sem }

// Global config

var (
	apiKey          string
	cmdPath         string
	port            string
	maxRetries      int
	retryDelay      time.Duration
	requestTimeout time.Duration
	maxRequestSize  int64
	maxTurns        int
	workerPool      *WorkerPool
	availableModels []ModelInfo
)

func main() {
	apiKey = os.Getenv("PROXY_API_KEY")
	if apiKey == "" {
		log.Fatal("PROXY_API_KEY environment variable required")
	}

	cmdPath = getEnvOrDefault("CMD_PATH", "cmd")
	port = getEnvOrDefault("PORT", "8080")
	maxRetries = getEnvIntOrDefault("MAX_RETRIES", 3)
	retryDelay = time.Duration(getEnvIntOrDefault("RETRY_DELAY_MS", 1000)) * time.Millisecond
	requestTimeout = time.Duration(getEnvIntOrDefault("REQUEST_TIMEOUT_SEC", 300)) * time.Second
	maxRequestSize = int64(getEnvIntOrDefault("MAX_REQUEST_SIZE_MB", 10)) * 1024 * 1024
	maxTurns = getEnvIntOrDefault("MAX_TURNS", 10)

	workerPool = NewWorkerPool(getEnvIntOrDefault("MAX_CONCURRENT", 4))
	availableModels = fetchModels()

	// Verify cmd is accessible
	if err := checkCmd(); err != nil {
		log.Fatalf("cmd not available: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/v1/models", handleModels)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      corsMiddleware(loggingMiddleware(mux)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	log.Printf("CommandCode Proxy starting on :%s (timeout=%v, max_turns=%d, models=%d)",
		port, requestTimeout, maxTurns, len(availableModels))
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Println("Server stopped")
}

// checkCmd verifies cmd is installed and accessible
func checkCmd() error {
	out, err := exec.Command(cmdPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("cmd not found at %s: %w", cmdPath, err)
	}
	log.Printf("cmd version: %s", strings.TrimSpace(string(out)))
	return nil
}

// Middleware

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Conversation-ID")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/health" {
			log.Printf("%s %s %v", r.Method, r.URL.Path, time.Since(start))
		}
	})
}

// Helpers

func fetchModels() []ModelInfo {
	out, err := exec.Command(cmdPath, "--list-models").Output()
	if err != nil {
		log.Printf("Warning: could not list models: %v", err)
		return []ModelInfo{{ID: "commandcode", OwnedBy: "commandcode"}}
	}
	var models []ModelInfo
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Available") || strings.HasPrefix(line, "Pass the") || strings.HasPrefix(line, "Docs:") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 1 {
			continue
		}
		id := parts[0]
		if id == "Open" || id == "Anthropic" || id == "OpenAI" || id == "Google" || id == "Sakana" {
			continue
		}
		models = append(models, ModelInfo{ID: id, OwnedBy: "commandcode", Description: strings.Join(parts[1:], " ")})
	}
	return models
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Handlers

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "ok",
		"cmd":            cmdPath,
		"models":         len(availableModels),
		"max_concurrent": cap(workerPool.sem),
	})
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": availableModels})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		sendError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != apiKey {
		sendError(w, "Invalid API key", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		sendError(w, "Failed to read request", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxRequestSize {
		sendError(w, "Request too large", http.StatusRequestEntityTooLarge)
		return
	}

	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	model := req.Model
	sessionID := r.Header.Get("X-Conversation-ID")
	if sessionID == "" {
		if parts := strings.SplitN(model, ":", 2); len(parts) == 2 {
			sessionID = parts[1]
			model = parts[0]
		}
	}
	model = strings.TrimPrefix(model, "commandcode/")

	systemPrompt, userPrompt := extractPrompts(req.Messages)
	prompt := buildPrompt(systemPrompt, userPrompt)

	// Build cmd options from request
	opts := cmdOptions{
		Continue:       req.Continue,
		Plan:           req.Plan,
		PermissionMode: req.PermissionMode,
		AddDir:         req.AddDir,
		ForkSession:    req.ForkSession,
	}

	log.Printf("Request: model=%s stream=%v msgs=%d session=%s prompt_len=%d opts=%+v",
		model, req.Stream, len(req.Messages), sessionID, len(prompt), opts)

	if req.Stream {
		handleStreaming(w, prompt, model, sessionID, opts)
	} else {
		handleNonStreaming(w, prompt, model, sessionID, opts)
	}
}

// extractPrompts

func extractPrompts(messages []Message) (system, user string) {
	var systemParts, userParts []string
	for _, msg := range messages {
		text := extractText(msg.Content)
		switch msg.Role {
		case "system":
			systemParts = append(systemParts, text)
		case "user":
			userParts = append(userParts, text)
		case "assistant":
			userParts = append(userParts, "[Previous assistant response: "+text+"]")
		}
	}
	return strings.Join(systemParts, "\n\n"), strings.Join(userParts, "\n\n")
}

func extractText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, block := range v {
			if m, ok := block.(map[string]interface{}); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", content)
	}
}

func buildPrompt(system, user string) string {
	if system != "" {
		return system + "\n\n" + user
	}
	return user
}

// cmd execution

type cmdOptions struct {
	Continue       bool
	Plan           bool
	PermissionMode string
	AddDir         string
	ForkSession    bool
}

type cmdResult struct {
	stdout string
	stderr string
	err    error
}

func runCmdWithRetry(ctx context.Context, prompt string, model string, sessionID string, opts cmdOptions) cmdResult {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return cmdResult{err: ctx.Err()}
		}
		if attempt > 0 {
			delay := retryDelay * time.Duration(1<<(attempt-1))
			log.Printf("Retry %d/%d after %v", attempt, maxRetries, delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return cmdResult{err: ctx.Err()}
			}
		}
		result := runCmd(ctx, prompt, model, sessionID, opts)
		if result.err == nil {
			return result
		}
		lastErr = result.err
		log.Printf("Attempt %d failed: %v", attempt+1, result.err)
	}
	return cmdResult{err: fmt.Errorf("all %d attempts failed: %w", maxRetries+1, lastErr)}
}

func runCmd(ctx context.Context, prompt string, model string, sessionID string, opts cmdOptions) cmdResult {
	workerPool.Acquire()
	defer workerPool.Release()

	args := buildArgs(prompt, model, sessionID, opts)
	cmd := exec.CommandContext(ctx, cmdPath, args...)
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return cmdResult{err: fmt.Errorf("stdout pipe: %w", err)}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return cmdResult{err: fmt.Errorf("stderr pipe: %w", err)}
	}

	if err := cmd.Start(); err != nil {
		return cmdResult{err: fmt.Errorf("cmd start: %w", err)}
	}

	var stdoutBuf, stderrBuf strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if stdoutBuf.Len() > 0 {
				stdoutBuf.WriteString("\n")
			}
			stdoutBuf.WriteString(scanner.Text())
		}
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if stderrBuf.Len() > 0 {
				stderrBuf.WriteString("\n")
			}
			stderrBuf.WriteString(scanner.Text())
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()

	return cmdResult{
		stdout: strings.TrimSpace(stdoutBuf.String()),
		stderr: strings.TrimSpace(stderrBuf.String()),
		err:    waitErr,
	}
}

func buildArgs(prompt string, model string, sessionID string, opts cmdOptions) []string {
	args := []string{"-p", prompt, "--verbose", "--max-turns", strconv.Itoa(maxTurns)}

	// Permission mode: default to yolo (auto-accept) for API usage
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", opts.PermissionMode)
	} else {
		args = append(args, "--yolo")
	}

	if model != "" {
		args = append(args, "-m", model)
	}

	// Session handling
	if opts.Continue {
		args = append(args, "--continue")
	} else if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	if opts.ForkSession {
		args = append(args, "--fork-session")
	}

	if opts.Plan {
		args = append(args, "--plan")
	}

	if opts.AddDir != "" {
		args = append(args, "--add-dir", opts.AddDir)
	}

	return args
}

func generateResponseID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

// Non-streaming

func handleNonStreaming(w http.ResponseWriter, prompt string, model string, sessionID string, opts cmdOptions) {
	w.Header().Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	start := time.Now()
	result := runCmdWithRetry(ctx, prompt, model, sessionID, opts)
	if result.err != nil {
		log.Printf("cmd error: %v", result.err)
		if ctx.Err() != nil {
			sendError(w, "Request timed out", http.StatusGatewayTimeout)
		} else {
			sendError(w, "cmd execution failed: "+result.err.Error(), http.StatusInternalServerError)
		}
		return
	}

	response := result.stdout
	log.Printf("Response: %d chars in %v", len(response), time.Since(start))

	// Extract session ID from stderr if cmd created a new session
	if sessionID == "" {
		if sid := parseSessionFromStderr(result.stderr); sid != "" {
			sessionID = sid
		}
	}

	resp := ChatResponse{
		ID: generateResponseID(), Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []Choice{{Index: 0, Message: &Message{Role: "assistant", Content: response}, FinishReason: "stop"}},
		Usage:   Usage{PromptTokens: len(prompt) / 4, CompletionTokens: len(response) / 4, TotalTokens: (len(prompt) + len(response)) / 4},
	}

	if sessionID != "" {
		w.Header().Set("X-Conversation-ID", sessionID)
	}
	json.NewEncoder(w).Encode(resp)
}

// Streaming

func handleStreaming(w http.ResponseWriter, prompt string, model string, sessionID string, opts cmdOptions) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	workerPool.Acquire()

	args := buildArgs(prompt, model, sessionID, opts)
	cmd := exec.CommandContext(ctx, cmdPath, args...)
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		workerPool.Release()
		sendSSEError(w, flusher, "Failed to start cmd")
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		workerPool.Release()
		sendSSEError(w, flusher, "Failed to start cmd stderr")
		return
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		workerPool.Release()
		sendSSEError(w, flusher, "Failed to start cmd: "+err.Error())
		return
	}

	chatID := generateResponseID()
	created := time.Now().Unix()

	sendSSEChunk(w, flusher, ChatResponse{
		ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []Choice{{Index: 0, Delta: &Delta{Role: "assistant"}}},
	})

	var fullResponse strings.Builder
	var stderrCapture strings.Builder

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			stderrCapture.WriteString(line)
			stderrCapture.WriteString("\n")
			if line == "" {
				continue
			}
			sendSSEChunk(w, flusher, ChatResponse{
				ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []Choice{{Index: 0, Delta: &Delta{Content: fmt.Sprintf("[tool] %s\n", line)}}},
			})
		}
	}()

	stdoutScanner := bufio.NewScanner(stdoutPipe)
	stdoutScanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for stdoutScanner.Scan() {
		line := stdoutScanner.Text()
		fullResponse.WriteString(line)
		fullResponse.WriteString("\n")
		sendSSEChunk(w, flusher, ChatResponse{
			ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []Choice{{Index: 0, Delta: &Delta{Content: line + "\n"}}},
		})
	}

	<-stderrDone

	// Extract session ID from stderr
	if sessionID == "" {
		if sid := parseSessionFromStderr(stderrCapture.String()); sid != "" {
			sessionID = sid
		}
	}

	if sessionID != "" {
		w.Header().Set("X-Conversation-ID", sessionID)
	}

	sendSSEChunk(w, flusher, ChatResponse{
		ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []Choice{{Index: 0, Delta: &Delta{}, FinishReason: "stop"}},
	})

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	cmd.Wait()
	workerPool.Release()
	log.Printf("Streaming done in %v (session=%s)", time.Since(start), sessionID)
}

// parseSessionFromStderr tries to extract session ID from cmd output
func parseSessionFromStderr(stderr string) string {
	// cmd may print session info to stderr, look for common patterns
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		// Look for session file path
		if strings.Contains(line, "session") && strings.Contains(line, ".json") {
			// Extract filename without extension
			parts := strings.Split(line, "/")
			if len(parts) > 0 {
			 fname := parts[len(parts)-1]
				fname = strings.TrimSuffix(fname, ".json")
				fname = strings.TrimSuffix(fname, ".jsonl")
				if fname != "" && fname != "session" {
					return fname
				}
			}
		}
	}
	return ""
}

// SSE helpers

func sendSSEChunk(w http.ResponseWriter, flusher http.Flusher, chunk ChatResponse) {
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func sendSSEError(w http.ResponseWriter, flusher http.Flusher, message string) {
	data, _ := json.Marshal(map[string]interface{}{
		"error": map[string]string{"message": message, "type": "error"},
	})
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func sendError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := ErrorResponse{}
	resp.Error.Message = message
	resp.Error.Type = "error"
	json.NewEncoder(w).Encode(resp)
}
