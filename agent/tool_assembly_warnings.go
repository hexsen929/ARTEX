package agent

import (
	"context"
	"strings"
	"sync"
)

type toolAssemblyWarningsKey struct{}

type toolAssemblyWarnings struct {
	mu       sync.Mutex
	messages []string
}

func withToolAssemblyWarnings(ctx context.Context) (context.Context, *toolAssemblyWarnings) {
	w := &toolAssemblyWarnings{}
	return context.WithValue(ctx, toolAssemblyWarningsKey{}, w), w
}

// AddToolAssemblyWarning records bounded host-authored diagnostic text for
// this assembly only. Do not pass model/source/error bodies or credentials.
// A caller outside AugmentTools has no collector and causes no global writes.
func AddToolAssemblyWarning(ctx context.Context, message string) {
	w, _ := ctx.Value(toolAssemblyWarningsKey{}).(*toolAssemblyWarnings)
	message = strings.TrimSpace(message)
	if w == nil || message == "" || len(message) > 512 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, prior := range w.messages {
		if prior == message {
			return
		}
	}
	if len(w.messages) < 16 {
		w.messages = append(w.messages, message)
	}
}

func (w *toolAssemblyWarnings) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.messages...)
}
