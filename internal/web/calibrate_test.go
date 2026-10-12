package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func testCalibrateOptions() CalibrateOptions {
	return CalibrateOptions{
		StartRPS:     25,
		Factor:       2,
		MaxRPS:       3200,
		Step:         400 * time.Millisecond,
		Pause:        0,
		MaxP99:       500 * time.Millisecond,
		MaxErrorRate: 0.01,
		MinAchieved:  0.8,
		MaxWorkers:   256,
		Timeout:      2 * time.Second,
	}
}

// Цель «выдерживает» 200 rps: всё, что сверх, получает 503. Калибровка обязана
// остановиться на пределе, а не уйти до потолка в 3200.
func TestCalibrateFindsLimit(t *testing.T) {
	limiter := rate.NewLimiter(200, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got, err := Calibrate(context.Background(), Target{ID: "t", URL: srv.URL}, testCalibrateOptions(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// Ступени 25, 50, 100, 200, 400: на 400 половина запросов получает 503.
	// 200 на грани (лимитер ровно на этой скорости), поэтому допускаем обе.
	if got != 100 && got != 200 {
		t.Errorf("max_rps = %d, ждали 100 или 200", got)
	}
}

// Быстрая цель: калибровка останавливается на потолке MaxRPS, а не крутится вечно.
func TestCalibrateRespectsCeiling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(okHandler))
	defer srv.Close()

	o := testCalibrateOptions()
	o.StartRPS, o.MaxRPS = 20, 60
	got, err := Calibrate(context.Background(), Target{ID: "t", URL: srv.URL}, o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got != 40 {
		t.Errorf("max_rps = %d, ждали 40 (ступени 20, 40, а 80 уже выше потолка 60)", got)
	}
}

// Мёртвая цель не проходит даже первую ступень.
func TestCalibrateDeadTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	got, err := Calibrate(context.Background(), Target{ID: "t", URL: srv.URL}, testCalibrateOptions(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("max_rps = %d, ждали 0", got)
	}
}

func TestNextStep(t *testing.T) {
	if got := nextStep(1, 1.25); got != 2 {
		t.Errorf("nextStep(1, 1.25) = %d, ждали 2: шаг хотя бы на 1", got)
	}
	if got := nextStep(100, 1.25); got != 125 {
		t.Errorf("nextStep(100, 1.25) = %d, ждали 125", got)
	}
}
