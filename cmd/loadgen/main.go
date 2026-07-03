package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/runner"
	"github.com/HollowKaeden/http-loadgen/internal/stats"
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

	printSummary(s)
}

// printSummary это временная печать итогов, пока нет пакета report. Показывает,
// что коллектор собрал: счётчики, RPS и разбивку по кодам и ошибкам.
func printSummary(s stats.Stats) {
	fmt.Printf("total=%d ok=%d failed=%d\n", s.Total, s.OK, s.Failed)
	if s.Elapsed > 0 {
		rps := float64(s.Total) / s.Elapsed.Seconds()
		fmt.Printf("elapsed=%s rps=%.1f\n", s.Elapsed.Round(time.Millisecond), rps)
	}

	if len(s.Latencies) > 0 {
		l := stats.Summarize(s.Latencies)
		fmt.Printf("latency: min=%s p50=%s p95=%s p99=%s max=%s\n",
			round(l.Min), round(l.P50), round(l.P95), round(l.P99), round(l.Max))
	}

	if len(s.ByStatus) > 0 {
		fmt.Print("by status:")
		for _, code := range sortedIntKeys(s.ByStatus) {
			fmt.Printf(" %d=%d", code, s.ByStatus[code])
		}
		fmt.Println()
	}

	if len(s.ByError) > 0 {
		fmt.Print("by error:")
		for _, kind := range sortedStringKeys(s.ByError) {
			fmt.Printf(" %s=%d", kind, s.ByError[kind])
		}
		fmt.Println()
	}
}

// round укорачивает латентность до микросекунд: наносекундный хвост в отчёте лишний,
// он только зашумляет числа и не несёт смысла на сетевых временах.
func round(d time.Duration) time.Duration {
	return d.Round(time.Microsecond)
}

func sortedIntKeys(m map[int]int) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func sortedStringKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
