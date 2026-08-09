package tmux

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestNudgeWithClaudeInboxFallsBackWhenSocketUnavailable(t *testing.T) {
	fallbackCalls := 0
	outcome, err := nudgeWithClaudeInbox("/var/tmp/gc-missing-claude-inbox.sock", runtime.TextContent("hello"), func() error {
		fallbackCalls++
		return nil
	})
	if err != nil {
		t.Fatalf("nudgeWithClaudeInbox: %v", err)
	}
	if outcome != runtime.NudgeOutcomeDelivered {
		t.Fatalf("outcome = %q, want delivered fallback", outcome)
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallbackCalls)
	}
}

func TestNudgeWithClaudeInboxReportsAcceptedSocketAsQueued(t *testing.T) {
	dir, err := os.MkdirTemp("/var/tmp", "gc-tmux-inbox-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "claude.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	received := make(chan string, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		var message struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&message); err != nil {
			serverErr <- err
			return
		}
		received <- message.Message.Content
	}()

	fallbackCalls := 0
	outcome, err := nudgeWithClaudeInbox(socketPath, runtime.TextContent("hello"), func() error {
		fallbackCalls++
		return nil
	})
	if err != nil {
		t.Fatalf("nudgeWithClaudeInbox: %v", err)
	}
	if outcome != runtime.NudgeOutcomeQueued {
		t.Fatalf("outcome = %q, want queued", outcome)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallbackCalls)
	}
	select {
	case err := <-serverErr:
		t.Fatalf("server: %v", err)
	case got := <-received:
		if got != "hello" {
			t.Fatalf("content = %q, want hello", got)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive inbox message")
	}
}

func TestNudgeWithClaudeInboxReturnsFallbackError(t *testing.T) {
	wantErr := errors.New("legacy nudge failed")
	_, err := nudgeWithClaudeInbox("", runtime.TextContent("hello"), func() error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestClaudeInboxMessageRejectsFileBlocks(t *testing.T) {
	if message, ok := claudeInboxMessage([]runtime.ContentBlock{{Type: "file_path", Path: "/tmp/report.txt"}}); ok || message != "" {
		t.Fatalf("file block = (%q, %v), want empty and unsupported", message, ok)
	}
}
