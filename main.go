package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

func main() {
	opts, help, err := parseOptions(os.Args[1:], os.Getenv)
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
	a := &app{opts: opts, ctx: ctx, out: os.Stdout, errOut: os.Stderr, interrupt: func() { signalCode.Store(130); cancel() }}
	if err := a.run(); err != nil {
		code := errorCode(err)
		if n := signalCode.Load(); n != 0 {
			code = int(n)
		}
		a.ui.fatal(a.step, err, code)
		os.Exit(code)
	}
}
