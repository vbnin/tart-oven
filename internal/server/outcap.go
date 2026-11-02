package server

import (
	"context"
	"strings"
)

// execOutputLimitKey carries a per-call cap on each captured stream (stdout and
// stderr) through the context of a guest command. Without it, output is
// unbounded, as it always was for the dashboard's terminal.
type execOutputLimitKey struct{}

func withExecOutputLimit(ctx context.Context, limit int) context.Context {
	return context.WithValue(ctx, execOutputLimitKey{}, limit)
}

// capBuffer collects a stream up to a limit and silently drops the rest, still
// reporting every byte as written so the producing process never blocks on a
// full pipe.
type capBuffer struct {
	sb        strings.Builder
	limit     int // 0 = unlimited
	truncated bool
}

func newCapBuffer(ctx context.Context) *capBuffer {
	limit, _ := ctx.Value(execOutputLimitKey{}).(int)
	return &capBuffer{limit: limit}
}

func (b *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit > 0 {
		room := b.limit - b.sb.Len()
		if room <= 0 {
			b.truncated = true
			return n, nil
		}
		if len(p) > room {
			p = p[:room]
			b.truncated = true
		}
	}
	b.sb.Write(p)
	return n, nil
}

func (b *capBuffer) String() string { return b.sb.String() }
