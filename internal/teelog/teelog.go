// Package teelog duplicates slog records to a second sink: the ops
// log. The bot keeps logging to stderr (docker logs) and additionally
// writes the same leveled records to BOTJE_LOG_DIR/ops.log, giving a
// persistent audit trail for admin logins, conf changes, reconnects,
// and module errors.
package teelog

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
)

type tee struct {
	a, b slog.Handler
}

// New returns a handler that forwards every record to both handlers.
func New(a, b slog.Handler) slog.Handler {
	return &tee{a: a, b: b}
}

func (t *tee) Enabled(ctx context.Context, level slog.Level) bool {
	return t.a.Enabled(ctx, level) || t.b.Enabled(ctx, level)
}

func (t *tee) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if t.a.Enabled(ctx, r.Level) {
		err = t.a.Handle(ctx, r.Clone())
	}
	if t.b.Enabled(ctx, r.Level) {
		if e := t.b.Handle(ctx, r.Clone()); err == nil {
			err = e
		}
	}
	return err
}

func (t *tee) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &tee{a: t.a.WithAttrs(attrs), b: t.b.WithAttrs(attrs)}
}

func (t *tee) WithGroup(name string) slog.Handler {
	return &tee{a: t.a.WithGroup(name), b: t.b.WithGroup(name)}
}

// OpsLog opens dir/ops.log for appending (creating dir as needed) and
// returns primary teed with a plain text handler on it, plus a close
// func. The caller keeps using the returned handler for the process
// lifetime; ops.log stays a single append file so logrotate can do its
// thing.
func OpsLog(primary slog.Handler, dir string) (slog.Handler, func() error, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "ops.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return New(primary, slog.NewTextHandler(f, nil)), f.Close, nil
}
