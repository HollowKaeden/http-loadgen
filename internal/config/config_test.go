package config

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestParseValidModes(t *testing.T) {
	cases := map[string][]string{
		"режим -n": {"-url", "http://x", "-n", "10"},
		"режим -d": {"-url", "http://x", "-d", "5s"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(args, io.Discard); err != nil {
				t.Fatalf("хотели валидный конфиг, получили ошибку: %v", err)
			}
		})
	}
}

// валидация должна ловить все плохие комбинации флагов и возвращать ошибку.
func TestParseInvalid(t *testing.T) {
	cases := map[string][]string{
		"нет url":          {"-n", "10"},
		"нет режима":       {"-url", "http://x"},
		"оба режима":       {"-url", "http://x", "-n", "10", "-d", "5s"},
		"c <= 0":           {"-url", "http://x", "-n", "10", "-c", "0"},
		"n <= 0":           {"-url", "http://x", "-n", "0"},
		"плохой метод":     {"-url", "http://x", "-n", "10", "-method", "FETCH"},
		"timeout <= 0":     {"-url", "http://x", "-n", "10", "-timeout", "0"},
		"rps < 0":          {"-url", "http://x", "-n", "10", "-rps", "-1"},
		"кривой заголовок": {"-url", "http://x", "-n", "10", "-H", "нет двоеточия"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(args, io.Discard); err == nil {
				t.Fatalf("хотели ошибку валидации, но её нет")
			}
		})
	}
}

func TestParseHeadersAndMethodUpcase(t *testing.T) {
	cfg, err := Parse([]string{
		"-url", "http://x", "-n", "1",
		"-method", "post",
		"-H", "Authorization: Bearer t",
		"-H", "X-Trace: 1",
	}, io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Method != "POST" {
		t.Errorf("метод = %q, хотели POST (в верхнем регистре)", cfg.Method)
	}
	if got := cfg.Headers.Get("Authorization"); got != "Bearer t" {
		t.Errorf("Authorization = %q", got)
	}
	if got := cfg.Headers.Get("X-Trace"); got != "1" {
		t.Errorf("X-Trace = %q", got)
	}
}

// синтаксис @file: тело запроса читается из файла.
func TestParseBodyFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(path, []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse([]string{"-url", "http://x", "-n", "1", "-body", "@" + path}, io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Body != `{"k":"v"}` {
		t.Errorf("тело = %q, хотели содержимое файла", cfg.Body)
	}
}
