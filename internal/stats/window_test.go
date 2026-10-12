package stats

import (
	"errors"
	"testing"
	"time"
)

// fakeClock это часы для collectLive: время двигается только тогда, когда тест
// сам вызывает advance, поэтому длины интервалов известны заранее.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// Интервалы режутся по тикам: каждое окно считает только свои запросы, пустой
// интервал отдаётся нулём, хвост отдаётся при закрытии канала, а итог совпадает с Collect.
func TestCollectLiveWindows(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	results := make(chan Result)
	ticks := make(chan time.Time)

	var windows []Window
	done := make(chan Stats)
	go func() {
		done <- collectLive(results, ticks, clock.now, func(w Window) { windows = append(windows, w) })
	}()

	// Окно 0: два успешных и одна ошибка.
	results <- Result{Status: 200, Latency: 10 * time.Millisecond}
	results <- Result{Status: 200, Latency: 30 * time.Millisecond}
	results <- Result{Err: errors.New("connection refused"), Latency: 5 * time.Millisecond}
	clock.advance(time.Second)
	ticks <- clock.t

	// Окно 1: пустое.
	clock.advance(time.Second)
	ticks <- clock.t

	// Окно 2: неполное, закрывается вместе с каналом.
	results <- Result{Status: 500, Latency: 50 * time.Millisecond}
	clock.advance(500 * time.Millisecond)
	close(results)

	s := <-done

	if len(windows) != 3 {
		t.Fatalf("окон %d, хотели 3: %+v", len(windows), windows)
	}

	w0 := windows[0]
	if w0.Index != 0 || w0.Total != 3 || w0.OK != 2 || w0.Failed != 1 {
		t.Errorf("окно 0 = %+v, хотели Index 0, Total 3, OK 2, Failed 1", w0)
	}
	if w0.Elapsed != time.Second || w0.RPS() != 3 {
		t.Errorf("окно 0: Elapsed %s, RPS %.1f, хотели 1s и 3", w0.Elapsed, w0.RPS())
	}
	if w0.Latency.Max != 30*time.Millisecond || w0.Latency.Min != 5*time.Millisecond {
		t.Errorf("окно 0: латентность %+v, хотели min 5ms и max 30ms", w0.Latency)
	}

	if w1 := windows[1]; w1.Index != 1 || w1.Total != 0 || w1.RPS() != 0 || w1.ErrorRate() != 0 {
		t.Errorf("окно 1 = %+v, хотели пустое", w1)
	}

	w2 := windows[2]
	if w2.Index != 2 || w2.Total != 1 || w2.Failed != 1 || w2.ErrorRate() != 1 {
		t.Errorf("окно 2 = %+v, хотели один неуспешный запрос", w2)
	}
	if w2.Elapsed != 500*time.Millisecond || w2.RPS() != 2 {
		t.Errorf("окно 2: Elapsed %s, RPS %.1f, хотели 500ms и 2", w2.Elapsed, w2.RPS())
	}
	// Латентность окна 2 своя, а не накопленная с начала прогона.
	if w2.Latency.Min != 50*time.Millisecond {
		t.Errorf("окно 2: min %s, хотели 50ms", w2.Latency.Min)
	}

	// Итог тот же, что дал бы обычный Collect.
	if s.Total != 4 || s.OK != 2 || s.Failed != 2 || len(s.Latencies) != 4 {
		t.Errorf("итог Total/OK/Failed/len = %d/%d/%d/%d, хотели 4/2/2/4", s.Total, s.OK, s.Failed, len(s.Latencies))
	}
	if s.ByStatus[500] != 1 || s.ByError["connection refused"] != 1 {
		t.Errorf("гистограммы итога: %v %v", s.ByStatus, s.ByError)
	}
}

// Если запросов не было вовсе, хвостовое окно не отдаётся: пустой прогон не
// должен рисовать лишнюю точку.
func TestCollectLiveNoTrailingEmptyWindow(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	results := make(chan Result)
	close(results)

	calls := 0
	collectLive(results, make(chan time.Time), clock.now, func(Window) { calls++ })

	if calls != 0 {
		t.Errorf("onWindow вызван %d раз, хотели 0", calls)
	}
}
