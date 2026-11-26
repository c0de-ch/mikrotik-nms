package routeros

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// The go-routeros client logs every sentence it sends at Debug level —
// including "/login =name=… =password=…". Every client this package creates
// therefore logs through clientLogHandler, which
//
//   - is capped at Info: Debug records (the sentences) are never produced,
//     whatever level the process logger runs at; and
//   - drops any attribute that carries a login secret, at any level, should
//     the library ever log a sentence higher up.
//
// What is left goes to the process logger (slog.Default at the time of the
// record).
type clientLogHandler struct {
	attrs  []slog.Attr
	groups []string
}

// secretMarkers are the words that carry a login secret: the password of
// the post-6.43 login and the challenge response of the older one.
var secretMarkers = []string{"=password=", "=response="}

// clientLogBase is where the client log ends up; a variable for tests.
var clientLogBase = func() slog.Handler { return slog.Default().Handler() }

func (h clientLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo && clientLogBase().Enabled(ctx, level)
}

func (h clientLogHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if !carriesSecret(a) {
			clean.AddAttrs(a)
		}
		return true
	})
	base := clientLogBase()
	if len(h.attrs) > 0 {
		base = base.WithAttrs(h.attrs)
	}
	for _, g := range h.groups {
		base = base.WithGroup(g)
	}
	return base.Handle(ctx, clean)
}

func (h clientLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := append([]slog.Attr(nil), h.attrs...)
	for _, a := range attrs {
		if !carriesSecret(a) {
			kept = append(kept, a)
		}
	}
	return clientLogHandler{attrs: kept, groups: h.groups}
}

func (h clientLogHandler) WithGroup(name string) slog.Handler {
	return clientLogHandler{attrs: h.attrs, groups: append(append([]string(nil), h.groups...), name)}
}

// carriesSecret reports an attribute whose value — however it is typed or
// nested — contains a login secret.
func carriesSecret(a slog.Attr) bool {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		for _, ga := range v.Group() {
			if carriesSecret(ga) {
				return true
			}
		}
		return false
	}
	s := v.String()
	if v.Kind() == slog.KindAny {
		s = fmt.Sprint(v.Any())
	}
	for _, m := range secretMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
