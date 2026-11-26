package main

import (
	"context"
	"io"
	"os"
	"os/signal"

	"github.com/lonegunmanb/sofa/internal/cli"
)

func main() {
	os.Exit(mainExitCode())
}

func mainExitCode() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	restoreInterruptHandling(ctx, stop)
	return run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func restoreInterruptHandling(ctx context.Context, stop context.CancelFunc) {
	go func() {
		<-ctx.Done()
		stop()
	}()
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return cli.Run(ctx, args, stdin, stdout, stderr, cli.Options{})
}
