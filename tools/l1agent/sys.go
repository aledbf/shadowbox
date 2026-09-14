package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// echo writes one line to stdout in a single write, as the echo builtin does.
func echo(s string) { os.Stdout.WriteString(s + "\n") }

// say is the bash say(): echo "L1: $*".
func say(s string) { echo("L1: " + s) }

// shErr prints a message where bash would have printed one of its own
// ("$0: line N: ..."), on stderr.
func shErr(s string) { os.Stderr.WriteString(progName + ": " + s + "\n") }

// errText is an error the way strerror(3) spells it.
func errText(err error) string {
	var en syscall.Errno
	if errors.As(err, &en) {
		s := en.Error()
		if s != "" {
			return strings.ToUpper(s[:1]) + s[1:]
		}
	}
	return err.Error()
}

// exitStatus is $? for a finished command: the exit code, or 128+signal.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
	}
	return 1
}

// startFailure is what bash says, and returns, for a command it could not
// start.
func startFailure(name string, err error) (string, int) {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return name + ": command not found", 127
	case errors.Is(err, syscall.ENOENT):
		return name + ": No such file or directory", 127
	}
	return name + ": " + errText(err), 126
}

func command(argv []string, stdin, stdout, stderr *os.File) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	// Only *os.File values, and only non-nil ones: a file is handed to the
	// child as its descriptor with no copying goroutine in between, and a
	// nil one is /dev/null.
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	return cmd
}

// run runs argv to completion and returns its status.  nil for stdout or
// stderr is /dev/null.
func run(argv []string, stdin, stdout, stderr *os.File) int {
	cmd := command(argv, stdin, stdout, stderr)
	if err := cmd.Start(); err != nil {
		msg, rc := startFailure(argv[0], err)
		if stderr != nil {
			stderr.WriteString(progName + ": " + msg + "\n")
		}
		return rc
	}
	return exitStatus(cmd.Wait())
}

// Where a piped command's stderr goes.
type stderrTo int

const (
	stderrConsole stderrTo = iota // "cmd | ...": bash's own stderr
	stderrMerged                  // "cmd 2>&1 | ..."
	stderrDiscard                 // "cmd 2>/dev/null | ..."
)

// pipe starts argv as the left side of a pipeline.  The caller reads the
// returned end (as much of it as the pipeline would: a "head" stops early),
// closes it, and then calls wait -- in that order, so that a command still
// writing sees the pipe close, as it would when head exits, rather than
// blocking forever on a full pipe.
func pipe(argv []string, se stderrTo) (*os.File, func() int) {
	r, w, err := os.Pipe()
	if err != nil {
		shErr("pipe: " + errText(err))
		return nil, func() int { return 1 }
	}
	var stderr *os.File
	switch se {
	case stderrConsole:
		stderr = os.Stderr
	case stderrMerged:
		stderr = w
	}
	cmd := command(argv, os.Stdin, w, stderr)
	if err := cmd.Start(); err != nil {
		msg, rc := startFailure(argv[0], err)
		if stderr != nil {
			stderr.WriteString(progName + ": " + msg + "\n")
		}
		w.Close()
		return r, func() int { return rc }
	}
	w.Close()
	return r, func() int { return exitStatus(cmd.Wait()) }
}

// capture is $(argv) before the trailing newlines are stripped, or the
// input of a pipeline that reads everything: all of argv's stdout.
func capture(argv []string, se stderrTo) []byte {
	r, wait := pipe(argv, se)
	var b []byte
	if r != nil {
		b, _ = io.ReadAll(r)
		r.Close()
	}
	wait()
	return b
}

// streamPrefixed is "argv | sed 's/^/prefix/'", or with max >= 0
// "argv | head -max | sed ...".
func streamPrefixed(argv []string, se stderrTo, prefix string, max int) int {
	r, wait := pipe(argv, se)
	if r != nil {
		prefixStream(os.Stdout, prefix, r, max)
		r.Close()
	}
	return wait()
}

// filterPrefixed is "argv | grep ... | head -max | sed 's/^/prefix/'": the
// lines keep() accepts, newline terminated as grep leaves them.
func filterPrefixed(argv []string, se stderrTo, keep func([]byte) bool, max int, prefix string) int {
	r, wait := pipe(argv, se)
	if r != nil {
		var out []string
		eachLine(r, func(l []byte, _ bool) bool {
			if max >= 0 && len(out) >= max {
				return false
			}
			if keep(l) {
				out = append(out, string(l))
			}
			return max < 0 || len(out) < max
		})
		r.Close()
		writePrefixed(os.Stdout, prefix, out)
	}
	return wait()
}

// echoTo is `echo <data> > path`: opened as bash's ">" opens, written in
// one write.  It succeeds as the echo would.
func echoTo(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	f.Close()
	return err
}

// echoLoud is echoTo without the 2>/dev/null: a failure is reported the
// way bash reports it.
func echoLoud(path, data string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		shErr(path + ": " + errText(err))
		return
	}
	if _, err := f.WriteString(data); err != nil {
		shErr("echo: write error: " + errText(err))
	}
	f.Close()
}

// executable is [ -x path ].
func executable(path string) bool {
	return syscall.Access(path, 1 /* X_OK */) == nil
}

// exists is [ -e path ].
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isDir is [ -d path ].
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// nonEmpty is [ -s path ].
func nonEmpty(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() > 0
}

// poweroff is "poweroff -f".  It returns if poweroff does, and callers
// carry on as the bash did.
func poweroff() int {
	return run([]string{"poweroff", "-f"}, os.Stdin, os.Stdout, os.Stderr)
}
