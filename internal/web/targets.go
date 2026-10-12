// Package web это HTTP-сервис поверх генератора: посетитель сайта запускает короткий
// прогон по одной из заранее разрешённых целей и смотрит метрики вживую.
//
// Сервис намеренно ничего не принимает от клиента, кроме id цели, доли нагрузки и
// длительности: URL, метод, заголовки и тело берутся только из файла целей. Так его
// нельзя превратить в прокси для атаки на чужие адреса (SSRF).
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Target это одна разрешённая цель. Нагружается только GET без тела и заголовков.
type Target struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// URL может содержать {domain}: домен подставляется при загрузке, чтобы файл целей
	// не менялся при переезде на другой домен.
	URL string `json:"url"`
	// MaxRPS это измеренный калибровкой максимум цели на этом сервере. 0 значит «ещё не
	// калибровали», такую цель запускать нельзя.
	MaxRPS int `json:"max_rps"`
}

// Targets это содержимое файла целей.
type Targets struct {
	Targets []Target `json:"targets"`
}

var targetID = regexp.MustCompile(`^[a-z0-9-]+$`)

// LoadTargets читает файл целей и подставляет домен. Ошибка в файле должна ронять
// старт сервиса, а не всплывать посреди прогона, поэтому всё проверяем здесь.
func LoadTargets(path, domain string) (*Targets, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	}
	var t Targets
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}
	for i := range t.Targets {
		t.Targets[i].URL = strings.ReplaceAll(t.Targets[i].URL, "{domain}", domain)
	}
	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}

func (t *Targets) validate() error {
	if len(t.Targets) == 0 {
		return errors.New("нет ни одной цели")
	}
	seen := map[string]bool{}
	for _, tg := range t.Targets {
		if !targetID.MatchString(tg.ID) {
			return fmt.Errorf("некорректный id цели %q", tg.ID)
		}
		if seen[tg.ID] {
			return fmt.Errorf("цель %q указана дважды", tg.ID)
		}
		seen[tg.ID] = true
		u, err := url.Parse(tg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("цель %q: некорректный url %q", tg.ID, tg.URL)
		}
		if strings.Contains(tg.URL, "{domain}") {
			return fmt.Errorf("цель %q: домен не подставлен", tg.ID)
		}
		if tg.MaxRPS < 0 {
			return fmt.Errorf("цель %q: max_rps < 0", tg.ID)
		}
	}
	return nil
}

// Find возвращает цель по id.
func (t *Targets) Find(id string) (Target, bool) {
	for _, tg := range t.Targets {
		if tg.ID == id {
			return tg, true
		}
	}
	return Target{}, false
}

// Save пишет цели обратно в файл (нужно калибровке). Домен при этом снова заменяется
// на {domain}, чтобы файл остался переносимым.
func (t *Targets) Save(path, domain string) error {
	out := Targets{Targets: make([]Target, len(t.Targets))}
	for i, tg := range t.Targets {
		if domain != "" {
			tg.URL = strings.ReplaceAll(tg.URL, domain, "{domain}")
		}
		out.Targets[i] = tg
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	// Пишем во временный файл и переименовываем: файл не останется полузаписанным.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
