package main

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// newProgressWriter returns a progress callback that writes to w when w is a
// terminal. When w is not a terminal (piped), it returns nil so the importer
// skips progress reporting entirely.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func newProgressWriter(w io.Writer) func(done, total int) {
	if !isTerminal(w) {
		return nil
	}
	return func(done, total int) {
		if total == 0 {
			return
		}
		pct := float64(done) / float64(total) * 100
		fmt.Fprintf(w, "\r\033[K  importing... %d/%d (%.0f%%)", done, total, pct) //nolint:errcheck
		if done >= total {
			fmt.Fprintf(w, "\r\033[K") //nolint:errcheck
		}
	}
}
