package claudeinbox

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSendWritesClaudeUserMessage(t *testing.T) {
	dir := shortTempDir(t)
	socketPath := filepath.Join(dir, "claude.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	received := make(chan map[string]any, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		var message map[string]any
		if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&message); err != nil {
			serverErr <- err
			return
		}
		received <- message
	}()

	start := time.Now()
	if err := Send(socketPath, "check deploy status"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("Send took %s, want a sub-second local socket write", elapsed)
	}

	select {
	case err := <-serverErr:
		t.Fatalf("server: %v", err)
	case message := <-received:
		if got := message["type"]; got != "user" {
			t.Fatalf("type = %#v, want user", got)
		}
		body, ok := message["message"].(map[string]any)
		if !ok {
			t.Fatalf("message = %#v, want object", message["message"])
		}
		if got := body["role"]; got != "user" {
			t.Fatalf("role = %#v, want user", got)
		}
		if got := body["content"]; got != "check deploy status" {
			t.Fatalf("content = %#v, want check deploy status", got)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive a message")
	}
}

func TestSendRejectsMissingSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "missing.sock")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("test socket unexpectedly exists: %v", err)
	}
	if err := Send(path, "hello"); err == nil {
		t.Fatal("Send(missing socket) = nil, want error")
	}
}

func BenchmarkSend(b *testing.B) {
	dir, err := os.MkdirTemp("/var/tmp", "gc-ci-bench-")
	if err != nil {
		b.Fatalf("MkdirTemp: %v", err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "claude.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	b.Cleanup(func() { _ = listener.Close() })

	serverErr := make(chan error, 1)
	go func() {
		for i := 0; i < b.N; i++ {
			conn, err := listener.Accept()
			if err != nil {
				serverErr <- err
				return
			}
			var message userMessage
			err = json.NewDecoder(bufio.NewReader(conn)).Decode(&message)
			_ = conn.Close()
			if err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Send(socketPath, "benchmark"); err != nil {
			b.Fatalf("Send: %v", err)
		}
	}
	b.StopTimer()
	if err := <-serverErr; err != nil {
		b.Fatalf("server: %v", err)
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "gc-ci-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
