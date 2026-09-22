package main

import (
	"fmt"
	"io"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

// Progress muestra el avance por stderr: una línea que se reescribe si es
// una terminal, o una línea cada 5 s si no lo es (logs, cron, CI).
type Progress struct {
	out        io.Writer
	tty        bool
	quiet      bool
	verbose    bool
	start      time.Time
	total      int64
	filesTotal int64
	done       atomic.Int64
	files      atomic.Int64
	mu         sync.Mutex
	shown      bool
	stop       chan struct{}
	wg         sync.WaitGroup
}

func NewProgress(out io.Writer, tty, quiet bool, filesTotal, bytesTotal int64, verbose bool) *Progress {
	return &Progress{
		out: out, tty: tty, quiet: quiet, verbose: verbose,
		total: bytesTotal, filesTotal: filesTotal,
		start: time.Now(), stop: make(chan struct{}),
	}
}

func (p *Progress) active() bool { return !p.quiet && p.filesTotal > 0 }

func (p *Progress) Add(n int64) { p.done.Add(n) }
func (p *Progress) FileDone()   { p.files.Add(1) }

func (p *Progress) Start() {
	if !p.active() {
		return
	}
	interval := 200 * time.Millisecond
	if !p.tty {
		interval = 5 * time.Second
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.render()
			case <-p.stop:
				return
			}
		}
	}()
}

func (p *Progress) Stop() {
	if !p.active() {
		return
	}
	close(p.stop)
	p.wg.Wait()
	p.render()
	p.mu.Lock()
	if p.tty && p.shown {
		fmt.Fprint(p.out, "\n")
		p.shown = false
	}
	p.mu.Unlock()
}

// Logf imprime una línea sin pisar la barra de progreso (sólo con -v).
func (p *Progress) Logf(format string, args ...any) {
	if p.quiet || !p.verbose {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty && p.shown {
		fmt.Fprint(p.out, "\r\033[K")
	}
	fmt.Fprintf(p.out, format+"\n", args...)
}

func (p *Progress) render() {
	done, files := p.done.Load(), p.files.Load()
	el := time.Since(p.start).Seconds()
	rate := 0.0
	if el > 0 {
		rate = float64(done) / el
	}
	pct := 100.0
	if p.total > 0 {
		pct = float64(done) * 100 / float64(p.total)
		if pct > 100 {
			pct = 100
		}
	}
	line := fmt.Sprintf("%5.1f%%  %s / %s  %s/s  archivos %d/%d",
		pct, humanBytes(done), humanBytes(p.total), humanBytes(int64(rate)), files, p.filesTotal)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty {
		fmt.Fprintf(p.out, "\r%s\033[K", line)
		p.shown = true
	} else {
		fmt.Fprintln(p.out, line)
	}
}

// PrintPlan lista qué haría (o haría falta hacer) una sincronización.
//
//	+ nuevo   ~ modificado   m sólo permisos   - se borra   ? sólo en destino (sin --delete)
func PrintPlan(w io.Writer, p *Plan, deleteOn bool) {
	for i := range p.Actions {
		a := &p.Actions[i]
		name := displayName(a)
		switch a.Kind {
		case ActMkdir:
			fmt.Fprintf(w, "+ %s/\n", name)
		case ActLink:
			fmt.Fprintf(w, "+ %s -> %s\n", name, a.Src.Link)
		case ActFile:
			if a.Dst == nil || a.Replace {
				fmt.Fprintf(w, "+ %s  (%s)\n", name, humanBytes(a.Src.Size))
			} else if a.Delta != nil {
				fmt.Fprintf(w, "~ %s  (%s → %s, %s) — delta: %d/%d bloques, %s a mover\n",
					name, humanBytes(a.Dst.Size), humanBytes(a.Src.Size), a.Why,
					a.Delta.Changed, a.Delta.Blocks, humanBytes(a.Delta.Bytes))
			} else {
				fmt.Fprintf(w, "~ %s  (%s → %s, %s)\n", name, humanBytes(a.Dst.Size), humanBytes(a.Src.Size), a.Why)
			}
		case ActMeta:
			fmt.Fprintf(w, "m %s  (%s)\n", name, a.Why)
		case ActDelete:
			fmt.Fprintf(w, "- %s\n", name)
		}
	}
	if !deleteOn {
		for i := range p.Extra {
			fmt.Fprintf(w, "? %s  (sólo en destino; --delete lo borraría)\n", path.Clean(p.Extra[i].Path))
		}
	}
	if p.Excluded > 0 {
		fmt.Fprintf(w, "x %d entradas de origen ignoradas por --exclude\n", p.Excluded)
	}
}

func (p *Plan) Summary() string {
	if !p.HasChanges() && len(p.Extra) == 0 {
		return "Sin diferencias."
	}
	s := fmt.Sprintf("%d nuevos, %d modificados, %d directorios, %d enlaces, %d sólo-permisos, %d a borrar",
		p.NewFiles, p.ModFiles, p.Dirs, p.Links, p.Metas, p.Deletes)
	if len(p.Extra) > 0 && p.Deletes == 0 {
		s += fmt.Sprintf(", %d sólo en destino (no se tocan)", len(p.Extra))
	}
	if p.Files > 0 {
		if p.Deep {
			s += fmt.Sprintf(" — delta real: %s de contenido a mover (de %s en archivos)", humanBytes(p.DeltaBytes), humanBytes(p.Bytes))
		} else {
			s += fmt.Sprintf(" — hasta %s a transferir (el delta puede reducirlo; --deep lo calcula exacto)", humanBytes(p.Bytes))
		}
	}
	if p.Excluded > 0 {
		s += fmt.Sprintf(", %d entradas ignoradas por --exclude", p.Excluded)
	}
	return s
}

func printSummary(w io.Writer, p *Plan, s *Stats, wire int64) {
	fmt.Fprintf(w, "Listo en %.1fs — %d nuevos, %d modificados, %d sólo metadatos, %d enlaces, %d directorios, %d borrados",
		s.Elapsed.Seconds(), s.NewFiles.Load(), s.ModFiles.Load(), s.MetaOnly.Load(),
		s.Links.Load(), s.Dirs.Load(), s.Deleted.Load())
	if f := s.Failed.Load(); f > 0 {
		fmt.Fprintf(w, ", %d con error", f)
	}
	fmt.Fprintln(w)
	fb, sent, reused := s.FileBytes.Load(), s.SentBytes.Load(), s.ReusedBytes.Load()
	if fb > 0 || reused > 0 {
		line := fmt.Sprintf("Datos: %s en archivos → %s de contenido movido", humanBytes(fb), humanBytes(sent))
		if reused > 0 {
			line += fmt.Sprintf(", %s ya estaban en destino (delta/reanudación)", humanBytes(reused))
		}
		if fb > 0 && sent < fb {
			line += fmt.Sprintf(" — %.0f%% menos", 100*(1-float64(sent)/float64(fb)))
		}
		fmt.Fprintln(w, line)
	}
	if wire > 0 {
		fmt.Fprintf(w, "En el cable (incluye compresión y protocolo): %s\n", humanBytes(wire))
	}
}