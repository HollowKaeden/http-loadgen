package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/stats"
)

// в режиме -n делаем ровно N запросов: столько же должно уйти на сервер и осесть
// в статистике, все как OK.
func TestRunExactCountAllOK(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		URL:         srv.URL,
		Concurrency: 4,
		Requests:    100,
		RequestsSet: true,
		Timeout:     5 * time.Second,
		Method:      http.MethodGet,
	}

	s, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}

	if got := atomic.LoadInt64(&hits); got != 100 {
		t.Errorf("сервер получил %d запросов, хотели 100", got)
	}
	if s.Total != 100 || s.OK != 100 || s.Failed != 0 {
		t.Errorf("Total/OK/Failed = %d/%d/%d, хотели 100/100/0", s.Total, s.OK, s.Failed)
	}
	if s.ByStatus[200] != 100 {
		t.Errorf("ByStatus[200] = %d, хотели 100", s.ByStatus[200])
	}
	if s.Elapsed <= 0 {
		t.Errorf("Elapsed = %s, хотели > 0 (нужен для RPS)", s.Elapsed)
	}
	if len(s.Latencies) != 100 {
		t.Errorf("Latencies len = %d, хотели 100", len(s.Latencies))
	}
}

// сервер отвечает вперемешку 200 и 500 (плюс искусственная задержка на 5xx), чтобы
// проверить, что не-2xx честно едет в Failed, а коды бьются по гистограмме.
func TestRunRecordsFailedStatuses(t *testing.T) {
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// чётные запросы это медленные 500, нечётные быстрые 200. Счётчик атомарный,
		// так что ровно половина уйдёт в 500 независимо от порядка воркеров.
		if atomic.AddInt64(&n, 1)%2 == 0 {
			time.Sleep(2 * time.Millisecond)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		URL:         srv.URL,
		Concurrency: 4,
		Requests:    50,
		RequestsSet: true,
		Timeout:     5 * time.Second,
		Method:      http.MethodGet,
	}

	s, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}

	if s.Total != 50 {
		t.Errorf("Total = %d, хотели 50", s.Total)
	}
	if s.OK != 25 || s.Failed != 25 {
		t.Errorf("OK/Failed = %d/%d, хотели 25/25", s.OK, s.Failed)
	}
	if s.ByStatus[200] != 25 || s.ByStatus[500] != 25 {
		t.Errorf("ByStatus = %v, хотели 200->25, 500->25", s.ByStatus)
	}
}

// open-loop с -rps держит темп: даже против мгновенного сервера прогон не может
// закончиться быстрее, чем лимитер выдаёт токены. Проверяем нижнюю границу времени
// (она зависит только от rate, поэтому тест не флакует): closed-loop против такого
// сервера уложился бы в миллисекунды, а тут обязан растянуться.
func TestRunOpenLoopRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const rps, requests = 200, 50
	cfg := &config.Config{
		URL:         srv.URL,
		Concurrency: 10,
		Requests:    requests,
		RequestsSet: true,
		RPS:         rps,
		Timeout:     5 * time.Second,
		Method:      http.MethodGet,
	}

	s, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run вернул ошибку: %v", err)
	}

	if s.Total != requests || s.OK != requests {
		t.Errorf("Total/OK = %d/%d, хотели %d/%d", s.Total, s.OK, requests, requests)
	}
	// burst 1: после первого токена ждём (N-1) интервалов по 1/rps. Берём заведомо
	// заниженную границу, чтобы не ловить дрожание планировщика.
	minElapsed := time.Duration(requests-1) * time.Second / rps / 2
	if s.Elapsed < minElapsed {
		t.Errorf("Elapsed = %s, при rps=%d ожидали хотя бы %s", s.Elapsed, rps, minElapsed)
	}
}

// отмена ctx обязана прервать летящий запрос, а не ждать его. Хендлер блокируется до
// отмены запроса, поэтому если бы отмена не пробрасывалась, Run завис бы и тест упал
// по дедлайну (клиентский timeout выставлен заведомо больше дедлайна теста).
func TestRunCancelAbortsInflight(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := &config.Config{
		URL:         srv.URL,
		Concurrency: 2,
		Duration:    10 * time.Second,
		DurationSet: true,
		Timeout:     30 * time.Second,
		Method:      http.MethodGet,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan stats.Stats, 1)
	go func() {
		s, _ := Run(ctx, cfg)
		done <- s
	}()

	<-started // хотя бы один запрос дошёл до сервера и висит там
	cancel()

	select {
	case s := <-done:
		if s.Total == 0 {
			t.Error("после отмены нет ни одного учтённого исхода")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run не вернулся после отмены: летящий запрос не прервался")
	}
}

// RunLive режет прогон на окна: сумма запросов по окнам совпадает с итогом, а номера
// окон идут подряд. Режим -n, чтобы итог был детерминированным.
func TestRunLiveWindowsSumToTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		URL:         srv.URL,
		Concurrency: 4,
		Requests:    60,
		RequestsSet: true,
		RPS:         200, // растягиваем прогон на ~0.3 с, чтобы окон было несколько
		Timeout:     5 * time.Second,
		Method:      http.MethodGet,
	}

	var windows []stats.Window
	s, err := RunLive(context.Background(), cfg, 50*time.Millisecond, func(w stats.Window) {
		windows = append(windows, w)
	})
	if err != nil {
		t.Fatalf("RunLive вернул ошибку: %v", err)
	}

	sum := 0
	for i, w := range windows {
		if w.Index != i {
			t.Errorf("окно %d имеет Index %d", i, w.Index)
		}
		sum += w.Total
	}
	if sum != s.Total || s.Total != 60 {
		t.Errorf("сумма по окнам %d, итог %d, хотели 60 и 60", sum, s.Total)
	}
	if len(windows) < 2 {
		t.Errorf("окон %d, при ~0.3 с и интервале 50 мс ожидали несколько", len(windows))
	}
}

// Так работает аварийный тормоз веб-формы: решение принимается в колбэке окна, и
// отмена ctx оттуда же останавливает прогон задолго до -d.
func TestRunLiveCancelFromWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.Config{
		URL:         srv.URL,
		Concurrency: 2,
		Duration:    30 * time.Second,
		DurationSet: true,
		RPS:         100,
		Timeout:     5 * time.Second,
		Method:      http.MethodGet,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan stats.Stats, 1)
	go func() {
		s, _ := RunLive(ctx, cfg, 50*time.Millisecond, func(w stats.Window) {
			if w.Total > 0 && w.ErrorRate() > 0.5 {
				cancel()
			}
		})
		done <- s
	}()

	select {
	case s := <-done:
		if s.Elapsed >= cfg.Duration {
			t.Errorf("прогон шёл %s, тормоз должен был остановить его раньше %s", s.Elapsed, cfg.Duration)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunLive не остановился после отмены из колбэка окна")
	}
}
