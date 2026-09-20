package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// cfgSize formatea un tamaño para `config show` (256MiB, 64KiB...).
func cfgSize(n int64) string {
	switch {
	case n > 0 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n > 0 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n > 0 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}

// parseSize entiende "1048576", "64KiB", "256MiB", "1G", "20MB" (todas base 1024).
func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(strings.ToUpper(s))
	t = strings.TrimSuffix(t, "IB")
	t = strings.TrimSuffix(t, "B")
	mult := int64(1)
	if t != "" {
		switch t[len(t)-1] {
		case 'K':
			mult = 1 << 10
			t = t[:len(t)-1]
		case 'M':
			mult = 1 << 20
			t = t[:len(t)-1]
		case 'G':
			mult = 1 << 30
			t = t[:len(t)-1]
		case 'T':
			mult = 1 << 40
			t = t[:len(t)-1]
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("tamaño inválido: %q", s)
	}
	return int64(v * float64(mult)), nil
}

func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null también es un dispositivo de caracteres, pero no es una terminal.
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}

func isYes(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "s", "si", "sí", "y", "yes":
		return true
	}
	return false
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

// splitArgs separa una línea de comando respetando comillas simples/dobles y "\".
func splitArgs(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inTok := false
	var quote rune
	esc := false
	for _, r := range line {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
			inTok = true
		case r == '\\' && quote != '\'':
			esc = true
			inTok = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inTok = true
		case r == ' ' || r == '\t':
			if inTok {
				args = append(args, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if quote != 0 {
		return nil, errors.New("comilla sin cerrar")
	}
	if esc {
		cur.WriteRune('\\')
	}
	if inTok {
		args = append(args, cur.String())
	}
	return args, nil
}

// byteSem es un semáforo por bytes: acota la cantidad de datos "en vuelo"
// (max_inflight_bytes de la spec) entre todos los archivos y bloques.
type byteSem struct {
	mu    sync.Mutex
	cond  *sync.Cond
	avail int64
	cap   int64
}

func newByteSem(n int64) *byteSem {
	if n < 1 {
		n = 1
	}
	s := &byteSem{avail: n, cap: n}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// acquire bloquea hasta tener n bytes de presupuesto y devuelve lo realmente
// tomado (se recorta al tope para que un bloque grande nunca bloquee para siempre).
func (s *byteSem) acquire(n int64) int64 {
	if n > s.cap {
		n = s.cap
	}
	s.mu.Lock()
	for s.avail < n {
		s.cond.Wait()
	}
	s.avail -= n
	s.mu.Unlock()
	return n
}

func (s *byteSem) release(n int64) {
	s.mu.Lock()
	s.avail += n
	s.mu.Unlock()
	s.cond.Broadcast()
}

// limiter reparte un ancho de banda máximo (bytes/s) entre todos los workers.
type limiter struct {
	mu   sync.Mutex
	rate float64
	next time.Time
}

func newLimiter(bytesPerSec int64) *limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &limiter{rate: float64(bytesPerSec)}
}

// wait duerme lo necesario para respetar el límite. Es nil-safe.
func (l *limiter) wait(n int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	d := l.next.Sub(now)
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	l.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
}
