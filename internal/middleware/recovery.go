package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover turns a panic into a logged error and a 500 page, keeping the
// process alive. onPanic renders the error response when nothing has been
// written to the client yet.
func Recover(log *slog.Logger, onPanic http.HandlerFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic recovered",
					"error", v,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				if sw, ok := w.(interface{ Status() int }); ok && sw.Status() != 0 {
					return // response already started; nothing sensible to add
				}
				onPanic(w, r)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
