package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type commandError struct {
	command string
	code    int
	err     error
}

func (e *commandError) Error() string { return fmt.Sprintf("%s: %v", e.command, e.err) }
func (e *commandError) Unwrap() error { return e.err }

func errorCode(err error) int {
	var command *commandError
	if errors.As(err, &command) {
		return command.code
	}
	return 1
}

func lookupError(err error) string {
	return fmt.Sprintf("Error: %v\nExit code: %d", err, errorCode(err))
}

func commandName(bin string, args ...string) string {
	parts := append([]string{bin}, args...)
	for i, part := range parts {
		if strings.ContainsAny(part, " \t\n'\";$`\\") {
			parts[i] = "'" + strings.ReplaceAll(part, "'", "'\\''") + "'"
		}
	}
	return strings.Join(parts, " ")
}

type runner struct {
	ctx   context.Context
	goBin string
	env   []string
}

func newRunner(ctx context.Context) (*runner, error) {
	bin, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("Go is not installed or not available in PATH")
	}
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GOWORK=") && !strings.HasPrefix(entry, "GOTOOLCHAIN=") {
			env = append(env, entry)
		}
	}
	env = append(env, "GOWORK=off", "GOTOOLCHAIN=local")
	return &runner{ctx: ctx, goBin: bin, env: env}, nil
}

func (r *runner) run(dir string, stdout, stderr io.Writer, bin string, args ...string) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	cmd := exec.CommandContext(r.ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, r.env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Kill descendants too: make and shells must stop before restoring go.mod.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if r.ctx.Err() != nil {
			return &commandError{command: commandName(bin, args...), code: 1, err: r.ctx.Err()}
		}
		code := 1
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			code = exit.ExitCode()
		}
		return &commandError{command: commandName(bin, args...), code: code, err: err}
	}
	return nil
}

func (r *runner) query(dir string, args ...string) (string, string, error) {
	var out, log bytes.Buffer
	err := r.run(dir, &out, &log, r.goBin, args...)
	if err != nil {
		return strings.TrimRight(out.String(), "\n"), out.String() + log.String(), err
	}
	return strings.TrimRight(out.String(), "\n"), log.String(), err
}
