package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

func main() {
	opts, help, err := parseOptions(os.Args[0], os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		usage(os.Stderr, os.Args[0])
		os.Exit(1)
	}
	if help {
		usage(os.Stdout, os.Args[0])
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	var signalCode atomic.Int32
	go func() {
		select {
		case sig := <-signals:
			if sig == syscall.SIGTERM {
				signalCode.Store(143)
			} else {
				signalCode.Store(130)
			}
			cancel()
		case <-ctx.Done():
		}
	}()
	a := &app{opts: opts, ctx: ctx, out: os.Stdout, errOut: os.Stderr}
	if err := a.run(); err != nil {
		code := 1
		var command *commandError
		if errors.As(err, &command) {
			code = command.code
		}
		if n := signalCode.Load(); n != 0 {
			code = int(n)
		}
		fmt.Fprintln(os.Stderr, "\n========================================\nERROR\n========================================")
		fmt.Fprintf(os.Stderr, "Step      : %s\nError     : %v\nExit code : %d\n", a.step, err, code)
		fmt.Fprintln(os.Stderr, "========================================")
		os.Exit(code)
	}
}
