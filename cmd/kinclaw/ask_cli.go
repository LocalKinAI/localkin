package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/LocalKinAI/kinclaw/pkg/permission"
	"golang.org/x/term"
)

// cliAsker is the terminal approver: prints the request on stderr and
// reads one line from stdin. Used by the REPL and by -exec when stdin
// is a terminal; a piped -exec gets no asker and the gate denies with
// an explanation instead of hanging on a prompt nobody will answer.
type cliAsker struct{}

func (cliAsker) Ask(ctx context.Context, req permission.Request) (permission.Answer, error) {
	fmt.Fprintf(os.Stderr, "\n\033[33m⚡ %s\033[0m\n   \033[2m%s\033[0m\n", req.Summary, req.Reason)
	for k, v := range req.Params {
		if len(v) > 200 {
			v = v[:200] + "…"
		}
		fmt.Fprintf(os.Stderr, "   \033[2m%s: %s\033[0m\n", k, strings.ReplaceAll(v, "\n", "\\n"))
	}
	fmt.Fprint(os.Stderr, "\033[33m   Allow? [y] once  [a] always this session  [n] deny (default n): \033[0m")

	line, err := readCookedLine(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr)
		return permission.Deny, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return permission.AllowOnce, nil
	case "a", "always":
		return permission.AllowSession, nil
	default:
		return permission.Deny, nil
	}
}

// readCookedLine reads a line from stdin one byte at a time so nothing
// beyond the newline is buffered away from the raw-mode readline that
// takes over stdin again after the turn. Stops early if ctx is done.
func readCookedLine(ctx context.Context) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var b [1]byte
		var sb strings.Builder
		for {
			n, err := os.Stdin.Read(b[:])
			if n == 1 {
				if b[0] == '\n' {
					ch <- res{sb.String(), nil}
					return
				}
				if b[0] != '\r' {
					sb.WriteByte(b[0])
				}
			}
			if err != nil {
				ch <- res{sb.String(), err}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// stdinIsTerminal reports whether a human can answer a prompt.
func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
