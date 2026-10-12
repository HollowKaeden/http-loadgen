package web

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/config"
	"github.com/HollowKaeden/http-loadgen/internal/report"
	"github.com/HollowKaeden/http-loadgen/internal/runner"
	"github.com/HollowKaeden/http-loadgen/internal/stats"
)

// Причины досрочной остановки, они же уходят клиенту в итоговом событии.
const (
	StopBrake     = "brake"     // сработал аварийный тормоз
	StopAbandoned = "abandoned" // за прогоном никто не следит
	StopShutdown  = "shutdown"  // сервис останавливается
)

// Brake это пороги аварийного тормоза. Смотрим не на одну секунду, а на последние
// Windows окон: одиночный выброс не должен рвать прогон, устойчивая деградация должна.
type Brake struct {
	Windows      int           // сколько последних окон учитывать
	MaxErrorRate float64       // доля ошибок, выше которой тормозим (0.05 = 5%)
	MaxP95       time.Duration // p95 любого из окон выше этого порога тоже повод тормозить
	MinRequests  int           // меньше запросов в окнах ещё не статистика, не тормозим
}

// windowEvent это одно окно в том виде, в каком его получает клиент: числа в мс,
// имена коротко, чтобы SSE-сообщение раз в секунду было маленьким.
type windowEvent struct {
	Index     int     `json:"i"`
	TimeMs    int64   `json:"t_ms"` // конец окна от начала прогона
	RPS       float64 `json:"rps"`
	OK        int     `json:"ok"`
	Failed    int     `json:"failed"`
	ErrorRate float64 `json:"error_rate"`
	P50       float64 `json:"p50_ms"`
	P95       float64 `json:"p95_ms"`
	P99       float64 `json:"p99_ms"`
}

// doneEvent это итог прогона: тот же стабильный JSON, что печатает CLI с -json,
// плюс причина досрочной остановки, если она была.
type doneEvent struct {
	Report  report.Report `json:"report"`
	Stopped string        `json:"stopped,omitempty"`
	Reason  string        `json:"reason,omitempty"`
}

// event это готовое к отправке SSE-сообщение. Сериализуем один раз, а не на каждого зрителя.
type event struct {
	name string
	data []byte
}

// Run это один прогон. Метрики окон приходят из горутины-коллектора генератора,
// зрители подключаются из HTTP-хендлеров, поэтому общее состояние под мьютексом.
type Run struct {
	ID        string    `json:"id"`
	TargetID  string    `json:"target_id"`
	RPS       int       `json:"rps"`
	Share     int       `json:"share"`
	DurationS int       `json:"duration_s"`
	StartedAt time.Time `json:"started_at"`
	cancel    context.CancelFunc
	brake     Brake
	recent    []stats.Window // последние окна для тормоза; трогает только коллектор
	mu        sync.Mutex
	history   []event
	subs      map[chan event]struct{}
	done      bool
	finished  chan struct{} // закрывается, когда итог опубликован
	stopped   string
	reason    string
}

// subscribe подключает зрителя: возвращает уже накопленную историю (опоздавший видит
// график с начала) и канал для новых событий. Канал закрывается после итога или
// если зритель не успевает читать.
func (r *Run) subscribe() ([]event, chan event, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	history := append([]event(nil), r.history...)
	ch := make(chan event, 16)
	if r.done {
		close(ch)
		return history, ch, func() {}
	}
	r.subs[ch] = struct{}{}
	unsubscribe := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	}
	return history, ch, unsubscribe
}

// Done закрывается, когда прогон закончился и итог опубликован.
func (r *Run) Done() <-chan struct{} { return r.finished }

// viewers это сколько зрителей сейчас подключено.
func (r *Run) viewers() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

// publish кладёт событие в историю и рассылает зрителям. Вызывается из горутины-
// коллектора, поэтому не блокируется: зрителя с переполненным буфером отключаем,
// он переподключится и получит историю заново.
func (r *Run) publish(e event, last bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.history = append(r.history, e)
	for ch := range r.subs {
		select {
		case ch <- e:
		default:
			delete(r.subs, ch)
			close(ch)
		}
	}
	if last {
		r.done = true
		close(r.finished)
		for ch := range r.subs {
			delete(r.subs, ch)
			close(ch)
		}
	}
}

// stop отменяет прогон и запоминает причину. Первая причина побеждает.
func (r *Run) stop(why, reason string) {
	r.mu.Lock()
	if r.stopped == "" {
		r.stopped, r.reason = why, reason
	}
	r.mu.Unlock()
	r.cancel()
}

// minWindow это самое короткое окно, которое стоит показывать. Хвост прогона после
// последнего целого интервала длится миллисекунды, и его RPS (1 запрос за 15 мс это
// «66 rps») рисовал бы на графике ложный всплеск. В итоговый отчёт он всё равно входит.
const minWindow = 500 * time.Millisecond

// onWindow вызывается коллектором раз в секунду: рассылаем окно и решаем, не тормозить ли.
func (r *Run) onWindow(w stats.Window) {
	if w.Elapsed < minWindow {
		return
	}
	e := windowEvent{
		Index:     w.Index,
		TimeMs:    time.Since(r.StartedAt).Milliseconds(),
		RPS:       round2(w.RPS()),
		OK:        w.OK,
		Failed:    w.Failed,
		ErrorRate: round2(w.ErrorRate()),
		P50:       ms(w.Latency.P50),
		P95:       ms(w.Latency.P95),
		P99:       ms(w.Latency.P99),
	}
	data, _ := json.Marshal(e)
	r.publish(event{name: "window", data: data}, false)

	if why := r.checkBrake(w); why != "" {
		r.stop(StopBrake, why)
	}
}

// checkBrake смотрит на последние окна и возвращает причину остановки или "".
func (r *Run) checkBrake(w stats.Window) string {
	r.recent = append(r.recent, w)
	if len(r.recent) > r.brake.Windows {
		r.recent = r.recent[1:]
	}

	total, failed := 0, 0
	var worstP95 time.Duration
	for _, x := range r.recent {
		total += x.Total
		failed += x.Failed
		if x.Latency.P95 > worstP95 {
			worstP95 = x.Latency.P95
		}
	}
	if total < r.brake.MinRequests {
		return ""
	}
	if rate := float64(failed) / float64(total); rate > r.brake.MaxErrorRate {
		return fmt.Sprintf("доля ошибок %.0f%% за последние секунды, порог %.0f%%", rate*100, r.brake.MaxErrorRate*100)
	}
	if worstP95 > r.brake.MaxP95 {
		return fmt.Sprintf("p95 %d мс, порог %d мс", worstP95.Milliseconds(), r.brake.MaxP95.Milliseconds())
	}
	return ""
}

// execute гоняет прогон до конца и публикует итог. Блокирует, зовётся в отдельной горутине.
func (r *Run) execute(ctx context.Context, cfg *config.Config) {
	s, err := runner.RunLive(ctx, cfg, time.Second, r.onWindow)

	r.mu.Lock()
	d := doneEvent{Stopped: r.stopped, Reason: r.reason}
	r.mu.Unlock()
	switch {
	case err != nil:
		d.Stopped, d.Reason = "error", err.Error()
	case d.Stopped == "" && ctx.Err() != nil:
		// ctx отменил не тормоз и не уход зрителей, значит, останавливается сам сервис
		d.Stopped, d.Reason = StopShutdown, "сервис перезапускается"
	}
	d.Report = report.Build(s)

	data, _ := json.Marshal(d)
	r.publish(event{name: "done", data: data}, true)
}

func ms(d time.Duration) float64 { return round2(float64(d) / float64(time.Millisecond)) }

func round2(x float64) float64 { return float64(int64(x*100+0.5)) / 100 }
