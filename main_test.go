package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func setupTestEnv(t *testing.T) {
	t.Helper()
	os.Setenv("PROXY_API_KEY", "test-key-123")
	os.Setenv("CMD_PATH", "/bin/echo")
	os.Setenv("MAX_RETRIES", "1")
	os.Setenv("RETRY_DELAY_MS", "10")
	os.Setenv("MAX_CONCURRENT", "2")
	os.Setenv("REQUEST_TIMEOUT_SEC", "5")
	os.Setenv("MAX_TURNS", "5")

	apiKey = "test-key-123"
	cmdPath = "/bin/echo"
	maxRetries = 1
	retryDelay = 10 * time.Millisecond
	requestTimeout = 5 * time.Second
	maxRequestSize = 10 * 1024 * 1024
	maxTurns = 5
	workerPool = NewWorkerPool(2)
	availableModels = []ModelInfo{
		{ID: "deepseek/deepseek-v4-pro", OwnedBy: "commandcode"},
		{ID: "claude-sonnet-4-6", OwnedBy: "commandcode"},
		{ID: "gpt-5.5", OwnedBy: "commandcode"},
	}
}

func TestHandleHealth(t *testing.T) {
	setupTestEnv(t)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	handleHealth(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
}

func TestHandleModels(t *testing.T) {
	setupTestEnv(t)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	handleModels(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data := resp["data"].([]interface{})
	if len(data) != 3 {
		t.Fatalf("expected 3 models, got %d", len(data))
	}
}

func TestHandleChat_Unauthorized(t *testing.T) {
	setupTestEnv(t)
	body := `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	handleChat(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleChat_BadMethod(t *testing.T) {
	setupTestEnv(t)
	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-key-123")
	w := httptest.NewRecorder()
	handleChat(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleChat_BadJSON(t *testing.T) {
	setupTestEnv(t)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString("not json"))
	req.Header.Set("Authorization", "Bearer test-key-123")
	w := httptest.NewRecorder()
	handleChat(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleChat_NonStreaming(t *testing.T) {
	setupTestEnv(t)
	body := `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"say hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer test-key-123")
	w := httptest.NewRecorder()
	handleChat(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp ChatResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		t.Fatal("expected message in choice")
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Fatalf("expected role assistant, got %v", resp.Choices[0].Message.Role)
	}
}

func TestHandleChat_SessionPassthrough(t *testing.T) {
	setupTestEnv(t)
	body := `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("X-Conversation-ID", "my-session-123")
	w := httptest.NewRecorder()
	handleChat(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHandleChat_ModelWithSession(t *testing.T) {
	setupTestEnv(t)
	body := `{"model":"deepseek-v4-pro:my-session","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer test-key-123")
	w := httptest.NewRecorder()
	handleChat(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestExtractPrompts(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "You are helpful"},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there"},
		{Role: "user", Content: "How are you?"},
	}
	system, user := extractPrompts(messages)
	if system != "You are helpful" {
		t.Fatalf("unexpected system: %q", system)
	}
	if user != "Hello\n\n[Previous assistant response: Hi there]\n\nHow are you?" {
		t.Fatalf("unexpected user: %q", user)
	}
}

func TestBuildArgs(t *testing.T) {
	args := buildArgs("test prompt", "my-model", "session-123", cmdOptions{})
	if args[0] != "-p" || args[1] != "test prompt" {
		t.Fatalf("unexpected args: %v", args)
	}
	if args[2] != "--verbose" {
		t.Fatalf("expected --verbose, got: %v", args)
	}
	// Check --max-turns
	found := false
	for i, a := range args {
		if a == "--max-turns" && i+1 < len(args) && args[i+1] == "5" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --max-turns 5, got: %v", args)
	}
	// Check --resume
	found = false
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) && args[i+1] == "session-123" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --resume session-123, got: %v", args)
	}
	// Check --yolo is default
	found = false
	for _, a := range args {
		if a == "--yolo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --yolo, got: %v", args)
	}
}

func TestBuildArgs_NoSession(t *testing.T) {
	args := buildArgs("prompt", "model", "", cmdOptions{})
	for i, a := range args {
		if a == "--resume" {
			t.Fatalf("should not have --resume when sessionID is empty, got: %v", args[i:])
		}
	}
}

func TestBuildArgs_Continue(t *testing.T) {
	args := buildArgs("prompt", "model", "", cmdOptions{Continue: true})
	found := false
	for _, a := range args {
		if a == "--continue" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --continue, got: %v", args)
	}
	// Should not have --resume when --continue is used
	for _, a := range args {
		if a == "--resume" {
			t.Fatalf("should not have --resume with --continue, got: %v", args)
		}
	}
}

func TestBuildArgs_Plan(t *testing.T) {
	args := buildArgs("prompt", "model", "", cmdOptions{Plan: true})
	found := false
	for _, a := range args {
		if a == "--plan" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --plan, got: %v", args)
	}
}

func TestBuildArgs_PermissionMode(t *testing.T) {
	args := buildArgs("prompt", "model", "", cmdOptions{PermissionMode: "standard"})
	found := false
	for i, a := range args {
		if a == "--permission-mode" && i+1 < len(args) && args[i+1] == "standard" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --permission-mode standard, got: %v", args)
	}
	// Should not have --yolo when permission mode is set
	for _, a := range args {
		if a == "--yolo" {
			t.Fatalf("should not have --yolo with --permission-mode, got: %v", args)
		}
	}
}

func TestBuildArgs_ForkSession(t *testing.T) {
	args := buildArgs("prompt", "model", "session-123", cmdOptions{ForkSession: true})
	found := false
	for _, a := range args {
		if a == "--fork-session" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --fork-session, got: %v", args)
	}
}

func TestBuildArgs_AddDir(t *testing.T) {
	args := buildArgs("prompt", "model", "", cmdOptions{AddDir: "/my/workspace"})
	found := false
	for i, a := range args {
		if a == "--add-dir" && i+1 < len(args) && args[i+1] == "/my/workspace" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --add-dir /my/workspace, got: %v", args)
	}
}

func TestWorkerPool(t *testing.T) {
	pool := NewWorkerPool(2)
	pool.Acquire()
	pool.Acquire()
	done := make(chan struct{})
	go func() {
		pool.Acquire()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("should have blocked")
	case <-time.After(50 * time.Millisecond):
	}
	pool.Release()
	pool.Release()
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("should have unblocked")
	}
	pool.Release()
}

func TestContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	time.Sleep(20 * time.Millisecond)
	if ctx.Err() == nil {
		t.Fatal("expected context to be done")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", ctx.Err())
	}
}

func TestGenerateID(t *testing.T) {
	id1 := generateID()
	id2 := generateID()
	if id1 == "" || id2 == "" {
		t.Fatal("expected non-empty IDs")
	}
	if id1 == id2 {
		t.Fatal("expected unique IDs")
	}
	if len(id1) != 16 {
		t.Fatalf("expected 16 char hex ID, got %d: %s", len(id1), id1)
	}
}

func TestParseSessionFromStderr(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		expected string
	}{
		{"empty", "", ""},
		{"no session", "some output\nmore output", ""},
		{"with json path", "Session saved to /home/user/.cmd/sessions/abc123.json", "abc123"},
		{"with jsonl path", "session: /tmp/sessions/test456.jsonl", "test456"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSessionFromStderr(tt.stderr)
			if result != tt.expected {
				t.Errorf("got %q, want %q", result, tt.expected)
			}
		})
	}
}
