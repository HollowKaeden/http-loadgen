// Package config описывает параметры прогона нагрузки и их парсинг из CLI-флагов.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config хранит полностью разобранную и провалидированную конфигурацию одного
// прогона. Дальше по коду его читают runner/worker/report, поэтому вся возня с
// флагами оседает здесь, а остальные слои получают уже готовые значения.
type Config struct {
	URL         string        // цель, обязателен
	Concurrency int           // -c, число воркеров
	Requests    int           // -n, всего запросов (0 => режим не выбран)
	Duration    time.Duration // -d, длительность прогона (0 => режим не выбран)
	Timeout     time.Duration // -timeout, таймаут одного запроса
	Method      string        // -method, HTTP-метод (в верхнем регистре)
	Body        string        // -body, тело запроса (уже с раскрытым @file)
	Headers     http.Header   // -H, повторяемый флаг
	JSON        bool          // -json, машиночитаемый вывод
	RPS         int           // -rps, целевая скорость open-loop (0 => closed-loop)

	// DurationSet/RequestsSet запоминают, был ли флаг реально указан пользователем.
	// Так мы отличаем "-n 0" от "-n вообще не передан", без этого не проверить
	// правило "ровно один из n/d".
	RequestsSet bool
	DurationSet bool
}

// allowedMethods это белый список HTTP-методов. Держим его тут, чтобы и валидация,
// и текст подсказки в ошибке брались из одного места.
var allowedMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// headerFlag реализует flag.Value для повторяемого флага -H "Key: Value".
// Пишем свою реализацию, потому что стандартный flag не умеет накапливать
// значения одного флага: каждый вызов Set просто добавляет ещё один заголовок.
type headerFlag struct {
	header http.Header
}

func (h *headerFlag) String() string {
	if h == nil || h.header == nil {
		return ""
	}
	var b strings.Builder
	for key, vals := range h.header {
		for _, v := range vals {
			fmt.Fprintf(&b, "%s: %s\n", key, v)
		}
	}
	return b.String()
}

func (h *headerFlag) Set(raw string) error {
	key, value, ok := strings.Cut(raw, ":")
	if !ok {
		return fmt.Errorf("заголовок %q не в формате \"Key: Value\"", raw)
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" {
		return fmt.Errorf("пустое имя заголовка в %q", raw)
	}
	h.header.Add(key, value)
	return nil
}

// Parse разбирает аргументы в Config и валидирует их. Вынесли это из main в
// отдельную функцию с явными args/output, чтобы гонять её в тестах без os.Args.
func Parse(args []string, output io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(output)

	cfg := &Config{Headers: http.Header{}}
	headers := &headerFlag{header: cfg.Headers}

	fs.StringVar(&cfg.URL, "url", "", "target URL (обязателен)")
	fs.IntVar(&cfg.Concurrency, "c", 50, "число воркеров / уровень конкурентности")
	fs.IntVar(&cfg.Requests, "n", 0, "всего запросов (взаимоисключающе с -d)")
	fs.DurationVar(&cfg.Duration, "d", 0, "длительность прогона, напр. 30s (взаимоисключающе с -n)")
	fs.DurationVar(&cfg.Timeout, "timeout", 10*time.Second, "таймаут одного запроса")
	fs.StringVar(&cfg.Method, "method", http.MethodGet, "HTTP-метод")
	fs.StringVar(&cfg.Body, "body", "", "тело запроса; @file читает тело из файла")
	fs.BoolVar(&cfg.JSON, "json", false, "вывести отчёт в JSON")
	fs.IntVar(&cfg.RPS, "rps", 0, "open-loop: целевая скорость запросов/сек (0 = closed-loop)")
	fs.Var(headers, "H", "заголовок \"Key: Value\", можно повторять")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// Запоминаем, какие из взаимоисключающих флагов реально передали.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "n":
			cfg.RequestsSet = true
		case "d":
			cfg.DurationSet = true
		}
	})

	cfg.Method = strings.ToUpper(cfg.Method)

	if err := cfg.resolveBody(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolveBody раскрывает синтаксис @file: если тело начинается с @, читаем файл.
// Если нужен литеральный @ в начале тела, его пишут как \@, и тут снимаем экранирование.
func (c *Config) resolveBody() error {
	switch {
	case strings.HasPrefix(c.Body, "@"):
		path := c.Body[1:]
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("чтение тела из %q: %w", path, err)
		}
		c.Body = string(data)
	case strings.HasPrefix(c.Body, `\@`):
		c.Body = c.Body[1:]
	}
	return nil
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.URL) == "" {
		return errors.New("-url обязателен")
	}
	if c.Concurrency <= 0 {
		return fmt.Errorf("-c должен быть > 0, получено %d", c.Concurrency)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("-timeout должен быть > 0, получено %s", c.Timeout)
	}
	if c.RPS < 0 {
		return fmt.Errorf("-rps должен быть >= 0, получено %d", c.RPS)
	}

	// Ровно один из -n / -d.
	switch {
	case c.RequestsSet && c.DurationSet:
		return errors.New("-n и -d взаимоисключающи, укажи что-то одно")
	case !c.RequestsSet && !c.DurationSet:
		return errors.New("нужен ровно один из -n (число запросов) или -d (длительность)")
	case c.RequestsSet && c.Requests <= 0:
		return fmt.Errorf("-n должен быть > 0, получено %d", c.Requests)
	case c.DurationSet && c.Duration <= 0:
		return fmt.Errorf("-d должен быть > 0, получено %s", c.Duration)
	}

	if !allowedMethods[c.Method] {
		return fmt.Errorf("метод %q не поддерживается (разрешены: GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS)", c.Method)
	}
	return nil
}
