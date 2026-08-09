// Package claudeinbox sends local messages to Claude Code's per-session inbox.
package claudeinbox

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

const socketTimeout = 500 * time.Millisecond

type userMessage struct {
	Type    string      `json:"type"`
	Message messageBody `json:"message"`
}

type messageBody struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Send writes a plain-text user message to a Claude Code session inbox.
// Success means Claude accepted the message into its local inbox; it does not
// mean the receiving session has already read it.
func Send(socketPath, content string) error {
	conn, err := net.DialTimeout("unix", socketPath, socketTimeout)
	if err != nil {
		return fmt.Errorf("connecting to Claude inbox %q: %w", socketPath, err)
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(socketTimeout)); err != nil {
		return fmt.Errorf("setting Claude inbox write deadline: %w", err)
	}
	message := userMessage{
		Type: "user",
		Message: messageBody{
			Role:    "user",
			Content: content,
		},
	}
	if err := json.NewEncoder(conn).Encode(message); err != nil {
		return fmt.Errorf("writing Claude inbox message: %w", err)
	}
	return nil
}
