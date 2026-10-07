package standalone

import (
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func notifyTerm(c chan os.Signal) { signal.Notify(c, syscall.SIGTERM, os.Interrupt) }

func newLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }
