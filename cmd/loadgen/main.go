package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/report"
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

	s, err := runner.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}

	if err := report.Write(os.Stdout, s, cfg.JSON); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка печати отчёта:", err)
		os.Exit(1)
	}
}
