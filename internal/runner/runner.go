package runner

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/stats"
)

// Run поднимает пул из cfg.Concurrency воркеров и раздаёт им работу через канал
// jobs. Воркеры шлют исходы в results, откуда мы их пока считаем прямо здесь.
func Run(ctx context.Context, cfg *config.Config) (stats.Stats, error) {
	return RunLive(ctx, cfg, 0, nil)
}

// RunLive это Run, который вдобавок раз в interval отдаёт в onWindow метрики за
// прошедший интервал (см. stats.CollectLive). Нужен веб-сервису: живой график и
// аварийный тормоз. Остановить прогон досрочно можно, отменив ctx прямо из onWindow.
// С onWindow == nil это ровно Run, поэтому CLI работает как раньше.
func RunLive(ctx context.Context, cfg *config.Config, interval time.Duration, onWindow func(stats.Window)) (stats.Stats, error) {
	client := newClient(cfg)

	jobs := make(chan struct{})
	results := make(chan stats.Result, cfg.Concurrency)

	// Продюсеру даём отдельный ctx: в режиме -d он живёт ровно cfg.Duration и по
	// истечении сам останавливает раздачу. Воркеры при этом работают на исходном ctx,
	// поэтому летящие на момент дедлайна запросы спокойно дожимаются, а не рвутся.
	// SIGINT отменяет исходный ctx, а с ним и производный, так что Ctrl+C тормозит обоих.
	produceCtx := ctx
	if cfg.DurationSet {
		var cancel context.CancelFunc
		produceCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	// Засекаем время именно вокруг раздачи и обработки запросов: это знаменатель RPS.
	start := time.Now()
	go produce(produceCtx, cfg, jobs)

	var wg sync.WaitGroup
	wg.Add(cfg.Concurrency)
	for i := 0; i < cfg.Concurrency; i++ {
		go func() {
			defer wg.Done()
			worker(ctx, client, cfg, jobs, results)
		}()
	}

	// results закрывает тот, кто в него пишет. Пишут воркеры, но каждый по
	// отдельности не знает, последний ли он, поэтому закрытие отдаём отдельной
	// горутине: она ждёт всех через WaitGroup и закрывает канал, чтобы цикл ниже
	// корректно завершился.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Коллектор читает results в этой горутине, пока канал не закроется. Он
	// единственный потребитель, поэтому копит Stats без синхронизации.
	var s stats.Stats
	if onWindow != nil {
		s = stats.CollectLive(results, interval, onWindow)
	} else {
		s = stats.Collect(results)
	}
	s.Elapsed = time.Since(start)
	return s, nil
}

// produce раздаёт задания воркерам и закрывает jobs, когда работа кончилась.
// Форма остановки зависит от режима:
//
//	-n: раздать ровно cfg.Requests заданий.
//	-d: раздавать, пока ctx жив (ctx уже ограничен cfg.Duration в Run).
//
// Если задан -rps, работаем в open-loop: перед каждой выдачей ждём токен лимитера,
// то есть держим темп cfg.RPS запросов/сек независимо от того, как быстро отвечает
// сервер. Без -rps лимитера нет, и темп задают сами воркеры (closed-loop).
func produce(ctx context.Context, cfg *config.Config, jobs chan<- struct{}) {
	defer close(jobs)

	var limiter *rate.Limiter
	if cfg.RPS > 0 {
		// Burst 1 держит выдачу равномерной: токены не копятся, поэтому после паузы
		// продюсер не выстреливает пачкой, а возвращается к ровному шагу 1/RPS.
		limiter = rate.NewLimiter(rate.Limit(cfg.RPS), 1)
	}

	// send выдаёт одно задание. Возвращает false, если ctx отменён (истёк -d или
	// пришёл SIGINT) или воркеры не разбирают задания до отмены. И ожидание токена,
	// и сама передача в канал прерываются по ctx, поэтому produce не залипает.
	send := func() bool {
		if limiter != nil {
			if err := limiter.Wait(ctx); err != nil {
				return false
			}
		}
		select {
		case <-ctx.Done():
			return false
		case jobs <- struct{}{}:
			return true
		}
	}

	if cfg.DurationSet {
		for send() {
		}
		return
	}

	for i := 0; i < cfg.Requests; i++ {
		if !send() {
			return
		}
	}
}
