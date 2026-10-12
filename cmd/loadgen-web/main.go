// loadgen-web это HTTP-сервис над генератором для формы на сайте-портфолио:
// посетитель запускает короткий open-loop прогон по одной из разрешённых целей и
// смотрит метрики вживую. CLI (cmd/loadgen) остаётся отдельным и не меняется.
//
//	loadgen-web serve      поднять API (по умолчанию)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/web"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	default:
		err = fmt.Errorf("неизвестная команда %q (есть: serve)", cmd)
	}
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

// env возвращает переменную окружения или значение по умолчанию: в контейнере
// удобнее задавать настройки через environment, чем через флаги.
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", env("LISTEN", ":8080"), "адрес HTTP-сервера")
	targetsPath := fs.String("targets", env("TARGETS", "targets.json"), "файл целей")
	domain := fs.String("domain", env("DOMAIN", ""), "домен, подставляется вместо {domain} в целях")
	trustedProxy := fs.String("trusted-proxy", env("TRUSTED_PROXY", ""), "IP прокси, которому верим в X-Forwarded-For")
	cooldown := fs.Duration("cooldown", 3*time.Minute, "пауза после любого прогона (3m..5m)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if v := os.Getenv("COOLDOWN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("COOLDOWN: %w", err)
		}
		*cooldown = d
	}
	if *cooldown < 3*time.Minute || *cooldown > 5*time.Minute {
		return fmt.Errorf("cooldown должен быть от 3m до 5m, получено %s", *cooldown)
	}

	targets, err := web.LoadTargets(*targetsPath, *domain)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sched := web.NewScheduler(ctx, web.Settings{
		Cooldown:       *cooldown,
		MaxDuration:    15 * time.Second,
		MinShare:       10,
		MaxShare:       50,
		MaxWorkers:     64,
		RequestTimeout: 5 * time.Second,
		IPLimit:        2,
		IPWindow:       15 * time.Minute,
		AbandonAfter:   5 * time.Second,
		Brake: web.Brake{
			Windows:      3,
			MaxErrorRate: 0.05,
			MaxP95:       time.Second,
			MinRequests:  20,
		},
	}, targets)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           web.NewServer(sched, *trustedProxy).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("loadgen-web: слушаю %s, целей %d, пауза %s", *listen, len(targets.Targets), *cooldown)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// SIGTERM от docker: ctx уже отменён, текущий прогон останавливается; ждём
	// закрытия соединений, но не дольше нескольких секунд.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
