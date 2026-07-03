package stats

import (
	"context"
	"errors"
	"testing"
	"time"
)

// коллектор должен разложить смесь исходов: 2xx в OK, не-2xx и транспортные ошибки
// в Failed, плюс гистограммы по кодам и по типам ошибок.
func TestCollectCounts(t *testing.T) {
	in := []Result{
		{Status: 200, Latency: 1 * time.Millisecond},
		{Status: 200, Latency: 2 * time.Millisecond},
		{Status: 500, Latency: 3 * time.Millisecond},
		{Status: 404, Latency: 4 * time.Millisecond},
		{Err: errors.New("connection refused"), Latency: 5 * time.Millisecond},
	}

	results := make(chan Result, len(in))
	for _, r := range in {
		results <- r
	}
	close(results)

	s := Collect(results)

	if s.Total != 5 {
		t.Errorf("Total = %d, хотели 5", s.Total)
	}
	if s.OK != 2 {
		t.Errorf("OK = %d, хотели 2", s.OK)
	}
	if s.Failed != 3 {
		t.Errorf("Failed = %d, хотели 3 (500, 404, ошибка)", s.Failed)
	}
	if s.ByStatus[200] != 2 || s.ByStatus[500] != 1 || s.ByStatus[404] != 1 {
		t.Errorf("ByStatus = %v", s.ByStatus)
	}
	if s.ByError["connection refused"] != 1 {
		t.Errorf("ByError = %v", s.ByError)
	}
	// латентности копим у всех исходов, включая ошибку.
	if len(s.Latencies) != 5 {
		t.Errorf("Latencies len = %d, хотели 5", len(s.Latencies))
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"canceled", context.Canceled, "canceled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"refused", errors.New("dial tcp 127.0.0.1:80: connect: connection refused"), "connection refused"},
		{"reset", errors.New("read: connection reset by peer"), "connection reset"},
		{"dns", errors.New("dial tcp: lookup nope: no such host"), "dns error"},
		{"eof", errors.New("unexpected EOF"), "eof"},
		{"other", errors.New("что-то пошло не так"), "other"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyError(c.err); got != c.want {
				t.Errorf("classifyError(%q) = %q, хотели %q", c.err, got, c.want)
			}
		})
	}
}
