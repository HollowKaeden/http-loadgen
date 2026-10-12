package web

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/report"
	"github.com/HollowKaeden/http-loadgen/internal/runner"
)

// CalibrateOptions это параметры калибровки: ступенчатого роста open-loop нагрузки
// до первой ступени, на которой цель перестаёт справляться.
type CalibrateOptions struct {
	StartRPS     int           // первая ступень
	Factor       float64       // во сколько раз растёт следующая ступень
	MaxRPS       int           // выше не идём, даже если цель справляется
	Step         time.Duration // длительность одной ступени
	Pause        time.Duration // пауза между ступенями, чтобы цель остыла
	MaxP99       time.Duration // ступень пройдена, если p99 не выше...
	MaxErrorRate float64       // ...ошибок меньше этой доли...
	MinAchieved  float64       // ...и реально выдано хотя бы такая доля заданного темпа
	MaxWorkers   int           // верхняя граница пула на ступени
	Timeout      time.Duration // таймаут одного запроса
}

// stepResult это итог одной ступени, для печати и для решения.
type stepResult struct {
	rps    int
	rep    report.Report
	passed bool
	why    string
}

// Calibrate ищет максимальную пропускную способность цели: наибольший RPS, при
// котором p99 не выше порога, ошибок меньше порога и генератор держит темп.
// Ход калибровки печатается в log. Возвращает последнюю пройденную ступень, 0 если
// не прошла даже первая.
func Calibrate(ctx context.Context, tg Target, o CalibrateOptions, log io.Writer) (int, error) {
	fmt.Fprintf(log, "\n== %s (%s)\n", tg.ID, tg.URL)
	fmt.Fprintf(log, "%8s %10s %8s %9s %9s  %s\n", "rps", "факт rps", "ошибок", "p95 мс", "p99 мс", "итог")

	best := 0
	for rps := o.StartRPS; rps <= o.MaxRPS; rps = nextStep(rps, o.Factor) {
		res, err := calibrationStep(ctx, tg, rps, o)
		if err != nil {
			return best, err
		}
		verdict := "ok"
		if !res.passed {
			verdict = "стоп: " + res.why
		}
		fmt.Fprintf(log, "%8d %10.1f %7.2f%% %9.1f %9.1f  %s\n",
			rps, res.rep.RPS, errorRate(res.rep)*100, res.rep.Latency.P95, res.rep.Latency.P99, verdict)

		if !res.passed {
			break
		}
		best = rps

		select {
		case <-ctx.Done():
			return best, ctx.Err()
		case <-time.After(o.Pause):
		}
	}
	fmt.Fprintf(log, "max_rps = %d\n", best)
	return best, nil
}

func calibrationStep(ctx context.Context, tg Target, rps int, o CalibrateOptions) (stepResult, error) {
	cfg := &config.Config{
		URL:    tg.URL,
		Method: http.MethodGet,
		RPS:    rps,
		// На калибровке пул берём с запасом: если он упрётся сам, мы померим не цель,
		// а размер пула. Ограничение сверху нужно только чтобы не съесть всю память.
		Concurrency: min(max(rps, 8), o.MaxWorkers),
		Duration:    o.Step,
		DurationSet: true,
		Timeout:     o.Timeout,
		Headers:     http.Header{},
	}
	s, err := runner.Run(ctx, cfg)
	if err != nil {
		return stepResult{}, err
	}
	if ctx.Err() != nil {
		return stepResult{}, ctx.Err()
	}

	rep := report.Build(s)
	res := stepResult{rps: rps, rep: rep, passed: true}
	switch {
	case errorRate(rep) >= o.MaxErrorRate:
		res.passed, res.why = false, fmt.Sprintf("ошибок %.2f%%", errorRate(rep)*100)
	case time.Duration(rep.Latency.P99*float64(time.Millisecond)) > o.MaxP99:
		res.passed, res.why = false, fmt.Sprintf("p99 %.0f мс", rep.Latency.P99)
	case rep.RPS < float64(rps)*o.MinAchieved:
		// Цель отвечает так медленно, что пул не успевает выдавать заданный темп.
		res.passed, res.why = false, fmt.Sprintf("выдано только %.0f rps", rep.RPS)
	}
	return res, nil
}

// nextStep это следующая ступень: умножаем на Factor, но растём хотя бы на 1.
func nextStep(rps int, factor float64) int {
	return max(int(math.Ceil(float64(rps)*factor)), rps+1)
}

func errorRate(r report.Report) float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.Failed) / float64(r.Total)
}
