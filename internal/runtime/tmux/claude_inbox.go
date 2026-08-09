package tmux

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/claudeinbox"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

const claudeMessagingSocketEnv = "CLAUDE_CODE_MESSAGING_SOCKET"

func claudeInboxMessage(content []runtime.ContentBlock) (string, bool) {
	if len(content) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case "", "text":
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		default:
			// Claude's inbox accepts plain text only. Preserve the legacy
			// path for file blocks so staging and references keep working.
			return "", false
		}
	}
	message := strings.Join(parts, "\n")
	return message, message != ""
}

func nudgeWithClaudeInbox(socketPath string, content []runtime.ContentBlock, fallback func() error) (runtime.NudgeOutcome, error) {
	if message, ok := claudeInboxMessage(content); ok && strings.TrimSpace(socketPath) != "" {
		if err := claudeinbox.Send(socketPath, message); err == nil {
			return runtime.NudgeOutcomeQueued, nil
		}
	}
	if err := fallback(); err != nil {
		return "", err
	}
	return runtime.NudgeOutcomeDelivered, nil
}

func (p *Provider) claudeMessagingSocket(name string) string {
	providerName, err := p.tm.GetEnvironment(name, "GC_PROVIDER")
	if err != nil || strings.TrimSpace(providerName) != "claude" {
		return ""
	}
	sessionID, err := p.tm.GetEnvironment(name, "GC_SESSION_ID")
	if err != nil || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	socketPath, err := proctable.FindEnvironmentBySessionID(strings.TrimSpace(sessionID), claudeMessagingSocketEnv)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(socketPath)
}

func (p *Provider) nudgeWithClaudeInbox(name string, content []runtime.ContentBlock, fallback func() error) (runtime.NudgeOutcome, error) {
	return nudgeWithClaudeInbox(p.claudeMessagingSocket(name), content, fallback)
}
