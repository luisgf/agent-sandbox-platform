// Package safe keeps a panic in one goroutine from taking the whole node-agent,
// and with it every VM on the node, down.
package safe

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Recover is deferred at the top of a goroutine (defer safe.Recover(...)): a
// panic is logged once, with its stack, and the goroutine ends instead of the
// process. onPanic, when set, then runs with the panic value to clean up after
// whatever panicked; a panic inside it is logged too, not propagated.
func Recover(log *slog.Logger, what string, onPanic func(value any), attrs ...any) {
	v := recover()
	if v == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Error(what+" panicked", append(attrs, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))...)
	if onPanic == nil {
		return
	}
	defer func() {
		if again := recover(); again != nil {
			log.Error(what+": cleaning up after the panic panicked too", append(attrs, "panic", fmt.Sprint(again))...)
		}
	}()
	onPanic(v)
}
