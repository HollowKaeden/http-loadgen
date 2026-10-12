package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/HollowKaeden/http-loadgen/internal/config"
)

// Settings это ограничения сервиса. Все они глобальные, на всех посетителей сразу:
// цель не дать формой положить сайт, на котором она живёт.
type Settings struct {
	Cooldown       time.Duration // пауза после окончания любого прогона
	MaxDuration    time.Duration // самый длинный прогон
	MinShare       int           // доля от max_rps цели, %, нижняя граница
	MaxShare       int           // и верхняя: потолок, выше которого форма не пустит
	MaxWorkers     int           // верхняя граница пула воркеров
	RequestTimeout time.Duration // таймаут одного запроса
	IPLimit        int           // сколько прогонов может запустить один IP...
	IPWindow       time.Duration // ...за это время
	AbandonAfter   time.Duration // столько без зрителей, и прогон отменяется
	Brake          Brake
}

// BusyError это отказ в запуске: идёт прогон, пауза или исчерпан лимит IP.
// Хендлер превращает его в 429 с понятным текстом и временем ожидания.
type BusyError struct {
	Message    string
	RetryAfter time.Duration
}

func (e *BusyError) Error() string { return e.Message }

// Scheduler решает, можно ли запустить прогон, и держит текущий и последний прогоны.
// Состояние только в памяти: после рестарта сервиса пауза сбрасывается, это допустимо.
type Scheduler struct {
	settings Settings
	targets  *Targets
	now      func() time.Time // подменяется в тестах

	mu           sync.Mutex
	current      *Run
	last         *Run
	lastFinished time.Time
	byIP         map[string][]time.Time
	baseCtx      context.Context // отменяется при остановке сервиса
}

func NewScheduler(ctx context.Context, settings Settings, targets *Targets) *Scheduler {
	return &Scheduler{
		settings: settings,
		targets:  targets,
		now:      time.Now,
		byIP:     map[string][]time.Time{},
		baseCtx:  ctx,
	}
}

// Status это ответ GET /status.
type Status struct {
	Running    bool   `json:"running"`
	Run        *Run   `json:"run,omitempty"` // текущий прогон: к нему можно подключиться зрителем
	LastRunID  string `json:"last_run_id,omitempty"`
	NextRunInS int    `json:"next_run_in_s"` // через сколько секунд можно запускать
	CooldownS  int    `json:"cooldown_s"`
}

func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := Status{
		Running:    s.current != nil,
		Run:        s.current,
		NextRunInS: ceilSeconds(s.waitLocked()),
		CooldownS:  int(s.settings.Cooldown.Seconds()),
	}
	if s.last != nil {
		st.LastRunID = s.last.ID
	}
	return st
}

// waitLocked это сколько ждать до следующего возможного запуска (без учёта лимита IP).
// Пока идёт прогон, считаем остаток его длительности плюс паузу.
func (s *Scheduler) waitLocked() time.Duration {
	now := s.now()
	if s.current != nil {
		end := s.current.StartedAt.Add(time.Duration(s.current.DurationS) * time.Second)
		return max(end.Sub(now), 0) + s.settings.Cooldown
	}
	if s.lastFinished.IsZero() {
		return 0
	}
	return max(s.lastFinished.Add(s.settings.Cooldown).Sub(now), 0)
}

// Find возвращает текущий или последний прогон по id (зритель мог опоздать к концу).
func (s *Scheduler) Find(id string) (*Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range []*Run{s.current, s.last} {
		if r != nil && r.ID == id {
			return r, true
		}
	}
	return nil, false
}

// Start проверяет все ограничения и, если можно, запускает прогон в фоне.
func (s *Scheduler) Start(targetID string, share, durationS int, ip string) (*Run, error) {
	tg, ok := s.targets.Find(targetID)
	if !ok {
		return nil, fmt.Errorf("неизвестная цель %q", targetID)
	}
	if tg.MaxRPS <= 0 {
		return nil, fmt.Errorf("цель %q ещё не откалибрована", targetID)
	}
	if share < s.settings.MinShare || share > s.settings.MaxShare {
		return nil, fmt.Errorf("доля должна быть от %d до %d%%", s.settings.MinShare, s.settings.MaxShare)
	}
	maxS := int(s.settings.MaxDuration.Seconds())
	if durationS < 1 || durationS > maxS {
		return nil, fmt.Errorf("длительность должна быть от 1 до %d с", maxS)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	if s.current != nil {
		return nil, &BusyError{Message: "Сейчас уже идёт прогон, его можно смотреть вживую", RetryAfter: s.waitLocked()}
	}
	if wait := s.waitLocked(); wait > 0 {
		return nil, &BusyError{Message: "После прогона сайту нужно отдохнуть", RetryAfter: wait}
	}
	if wait := s.ipWaitLocked(ip, now); wait > 0 {
		return nil, &BusyError{
			Message:    fmt.Sprintf("С одного адреса не больше %d прогонов за %d мин", s.settings.IPLimit, int(s.settings.IPWindow.Minutes())),
			RetryAfter: wait,
		}
	}

	rps := int(math.Round(float64(tg.MaxRPS) * float64(share) / 100))
	rps = max(rps, 1)
	ctx, cancel := context.WithCancel(s.baseCtx)
	r := &Run{
		ID:        newID(),
		TargetID:  tg.ID,
		RPS:       rps,
		Share:     share,
		DurationS: durationS,
		StartedAt: now,
		cancel:    cancel,
		brake:     s.settings.Brake,
		subs:      map[chan event]struct{}{},
		finished:  make(chan struct{}),
	}
	cfg := &config.Config{
		URL:         tg.URL,
		Method:      http.MethodGet,
		RPS:         rps,
		Concurrency: workersFor(rps, s.settings.MaxWorkers),
		Duration:    time.Duration(durationS) * time.Second,
		DurationSet: true,
		Timeout:     s.settings.RequestTimeout,
		Headers:     http.Header{},
	}

	s.current = r
	s.byIP[ip] = append(s.byIP[ip], now)

	go s.watchViewers(ctx, r)
	go func() {
		r.execute(ctx, cfg)
		cancel()
		s.mu.Lock()
		s.current, s.last, s.lastFinished = nil, r, s.now()
		s.mu.Unlock()
	}()
	return r, nil
}

// ipWaitLocked чистит старые записи IP и говорит, сколько ему ждать (0 = можно).
func (s *Scheduler) ipWaitLocked(ip string, now time.Time) time.Duration {
	recent := s.byIP[ip][:0]
	for _, t := range s.byIP[ip] {
		if now.Sub(t) < s.settings.IPWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(s.byIP, ip)
	} else {
		s.byIP[ip] = recent
	}
	if len(recent) < s.settings.IPLimit {
		return 0
	}
	return recent[0].Add(s.settings.IPWindow).Sub(now)
}

// watchViewers отменяет прогон, если за ним никто не следит (посетитель закрыл
// вкладку). Первые AbandonAfter не проверяем: зрителю нужно время подключиться.
func (s *Scheduler) watchViewers(ctx context.Context, r *Run) {
	ticker := time.NewTicker(s.settings.AbandonAfter / 5)
	defer ticker.Stop()
	var alone time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.viewers() > 0 {
				alone = 0
				continue
			}
			alone += s.settings.AbandonAfter / 5
			if alone >= s.settings.AbandonAfter {
				r.stop(StopAbandoned, "за прогоном никто не следит")
				return
			}
		}
	}
}

// workersFor подбирает размер пула под темп. В open-loop пул ограничивает число
// одновременных запросов: при rps и латентности L нужно около rps*L воркеров.
// Берём запас на L до 500 мс, но не больше maxWorkers.
func workersFor(rps, maxWorkers int) int {
	return min(max(rps/2, 4), maxWorkers)
}

func ceilSeconds(d time.Duration) int { return int(math.Ceil(d.Seconds())) }

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
