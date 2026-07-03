package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/runner"
)

func main() {
	cfg, err := config.Parse(os.Args[1:], os.Stderr)
	if err != nil {
		// flag.ErrHelp значит, что usage уже напечатан по -h/-help, это не ошибка.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(2)
	}

	// SIGINT (Ctrl+C) отменяет ctx: продюсер перестаёт раздавать работу, а летящие
	// запросы обрываются. Так Ctrl+C мягко останавливает прогон без паники.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := runner.Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}
