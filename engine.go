package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options controla una corrida de sincronización.
type Options struct {
	Delete      bool
	MaxDelete   int // 0 = ningún borrado permitido; <0 = sin límite
	ConfirmMass bool
	ConfirmOver int
	Confirm     func(msg string) bool // nil = no se puede confirmar (se aborta)
	Workers     int
	MaxInflight int64
	BlockSize   int
	SmallFile   int64
	BWLimit     int64
	Resume      bool
	Verbose     bool
	Quiet       bool
	TTY         bool
	Out         io.Writer // planes y listados (stdout)
	Err         io.Writer // progreso y resumen (stderr)
}

type ActKind int

const (
	ActMkdir ActKind = iota
	ActFile
	ActLink
	ActMeta
	ActDelete
)

// Action es un paso del plan. Src/Dst son las entradas observadas
// (Dst == nil si no existe). Replace indica que Dst es de otro tipo y hay
// que quitarlo antes.
type Action struct {
	Kind    ActKind
	Rel     string
	SrcPath string
	DstPath string
	Src     *Entry
	Dst     *Entry
	Why     string
	Replace bool
}

type Plan struct {
	Actions []Action
	Extra   []Entry // sólo en destino (se borran únicamente con --delete)

	Files, NewFiles, ModFiles, Dirs, Links, Metas, Deletes int
	Bytes                                                  int64 // suma de tamaños de archivos a transferir (cota superior)
}

func (p *Plan) HasChanges() bool { return len(p.Actions) > 0 }

func (p *Plan) finish() {
	for i := range p.Actions {
		a := &p.Actions[i]
		switch a.Kind {
		case ActMkdir:
			p.Dirs++
		case ActLink:
			p.Links++
		case ActMeta:
			p.Metas++
		case ActDelete:
			p.Deletes++
		case ActFile:
			p.Files++
			p.Bytes += a.Src.Size
			if a.Dst == nil || a.Replace {
				p.NewFiles++
			} else {
				p.ModFiles++
			}
		}
	}
}

type Failure struct {
	Rel string
	Err error
}

// Stats acumula métricas de la corrida (contadores atómicos: se actualizan
// desde muchos workers).
type Stats struct {
	NewFiles, ModFiles, MetaOnly, Links, Dirs, Deleted, Failed atomic.Int64
	FileBytes                                                  atomic.Int64 // tamaño de los archivos procesados
	SentBytes                                                  atomic.Int64 // contenido realmente movido (post-delta, pre-compresión)
	ReusedBytes                                                atomic.Int64 // ya estaba en destino (delta / reanudación)
	Elapsed                                                    time.Duration
}

type Result struct {
	Plan     *Plan
	Stats    *Stats
	Failures []Failure
}

// ---------------------------------------------------------------- validación

// validRel rechaza rutas relativas peligrosas provenientes de un listado
// (que podría venir de un servidor no confiable).
func validRel(rel string) bool {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// checkEntries valida un listado: sin traversal y sin entradas "dentro" de
// un symlink (la forma clásica de escribir fuera del destino).
func checkEntries(list []Entry) error {
	links := map[string]bool{}
	for i := range list {
		if !validRel(list[i].Path) {
			return fmt.Errorf("ruta insegura en el listado: %q", list[i].Path)
		}
		if list[i].Type == TLink {
			links[list[i].Path] = true
		}
	}
	for i := range list {
		for p := path.Dir(list[i].Path); p != "."; p = path.Dir(p) {
			if links[p] {
				return fmt.Errorf("entrada %q está dentro del symlink %q", list[i].Path, p)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- planificación

// classify aplica el quick-check de la spec (tipo, tamaño, mtime, modo) y
// devuelve la acción necesaria, o nil si ya está al día.
func classify(s, d *Entry) *Action {
	switch s.Type {
	case TDir:
		if d == nil {
			return &Action{Kind: ActMkdir, Why: "nuevo"}
		}
		if d.Type != TDir {
			return &Action{Kind: ActMkdir, Why: "tipo distinto", Replace: true}
		}
		if d.Mode != s.Mode {
			return &Action{Kind: ActMeta, Why: "permisos"}
		}
	case TLink:
		if d == nil {
			return &Action{Kind: ActLink, Why: "nuevo"}
		}
		if d.Type != TLink {
			return &Action{Kind: ActLink, Why: "tipo distinto", Replace: true}
		}
		if d.Link != s.Link {
			return &Action{Kind: ActLink, Why: "destino del enlace", Replace: true}
		}
	case TFile:
		if d == nil {
			return &Action{Kind: ActFile, Why: "nuevo"}
		}
		if d.Type != TFile {
			return &Action{Kind: ActFile, Why: "tipo distinto", Replace: true}
		}
		if d.Size != s.Size {
			return &Action{Kind: ActFile, Why: "tamaño"}
		}
		if d.MTime != s.MTime {
			return &Action{Kind: ActFile, Why: "mtime"}
		}
		if d.Mode != s.Mode {
			return &Action{Kind: ActMeta, Why: "permisos"}
		}
	}
	return nil
}

// BuildPlan compara origen y destino sin modificar nada.
//
// Semántica de rutas (igual para put/get/sync/diff):
//   - origen directorio  -> el destino ES ese directorio (se fusiona su contenido; sin anidar).
//   - origen archivo     -> el destino es el nombre final, salvo que sea un directorio existente
//     (entonces va adentro, con el nombre del origen).
func BuildPlan(src FS, srcPath string, dst FS, dstPath string, o *Options) (*Plan, error) {
	sroot, err := src.Stat(srcPath, true)
	if err != nil {
		return nil, fmt.Errorf("origen %q: %w", srcPath, err)
	}
	droot, derr := dst.Stat(dstPath, true)
	if derr != nil && !errors.Is(derr, fs.ErrNotExist) {
		return nil, fmt.Errorf("destino %q: %w", dstPath, derr)
	}
	dstExists := derr == nil
	plan := &Plan{}

	switch sroot.Type {
	case TFile:
		target := dstPath
		var dcur *Entry
		if dstExists && droot.Type == TDir {
			target = path.Join(dstPath, path.Base(srcPath))
			d, serr := dst.Stat(target, false)
			if serr == nil {
				dcur = d
			} else if !errors.Is(serr, fs.ErrNotExist) {
				return nil, fmt.Errorf("destino %q: %w", target, serr)
			}
		} else if dstExists {
			d, serr := dst.Stat(dstPath, false)
			if serr != nil {
				return nil, fmt.Errorf("destino %q: %w", dstPath, serr)
			}
			dcur = d
		}
		if a := classify(sroot, dcur); a != nil {
			a.Rel, a.Src, a.Dst = ".", sroot, dcur
			a.SrcPath, a.DstPath = srcPath, target
			plan.Actions = append(plan.Actions, *a)
		}

	case TDir:
		if dstExists && droot.Type != TDir {
			return nil, fmt.Errorf("el destino %q existe y no es un directorio", dstPath)
		}
		slist, err := src.List(srcPath, true)
		if err != nil {
			return nil, fmt.Errorf("listando origen: %w", err)
		}
		if err := checkEntries(slist); err != nil {
			return nil, fmt.Errorf("listado del origen: %w", err)
		}
		var dlist []Entry
		if dstExists {
			dlist, err = dst.List(dstPath, true)
			if err != nil {
				return nil, fmt.Errorf("listando destino: %w", err)
			}
			if err := checkEntries(dlist); err != nil {
				return nil, fmt.Errorf("listado del destino: %w", err)
			}
		} else {
			plan.Actions = append(plan.Actions, Action{
				Kind: ActMkdir, Rel: ".", SrcPath: srcPath, DstPath: dstPath, Src: sroot, Why: "nuevo",
			})
		}
		dmap := make(map[string]*Entry, len(dlist))
		for i := range dlist {
			dmap[dlist[i].Path] = &dlist[i]
		}
		seen := make(map[string]bool, len(slist))
		var dirs, others []Action
		for i := range slist {
			s := &slist[i]
			seen[s.Path] = true
			d := dmap[s.Path]
			a := classify(s, d)
			if a == nil {
				continue
			}
			a.Rel, a.Src, a.Dst = s.Path, s, d
			a.SrcPath = path.Join(srcPath, s.Path)
			a.DstPath = path.Join(dstPath, s.Path)
			if a.Kind == ActMkdir {
				dirs = append(dirs, *a)
			} else {
				others = append(others, *a)
			}
		}
		plan.Actions = append(plan.Actions, dirs...)
		plan.Actions = append(plan.Actions, others...)
		for i := range dlist {
			if !seen[dlist[i].Path] {
				plan.Extra = append(plan.Extra, dlist[i])
			}
		}
		if o.Delete {
			extra := append([]Entry(nil), plan.Extra...)
			// Hijos antes que padres.
			sort.SliceStable(extra, func(i, j int) bool {
				return strings.Count(extra[i].Path, "/") > strings.Count(extra[j].Path, "/")
			})
			for i := range extra {
				e := &extra[i]
				plan.Actions = append(plan.Actions, Action{
					Kind: ActDelete, Rel: e.Path, DstPath: path.Join(dstPath, e.Path), Dst: e, Why: "sólo en destino",
				})
			}
		}

	default:
		return nil, fmt.Errorf("origen %q: tipo no soportado", srcPath)
	}
	plan.finish()
	return plan, nil
}

// checkSafety aplica las salvaguardas de borrado ANTES de tocar nada.
func checkSafety(p *Plan, o *Options) error {
	if p.Deletes == 0 {
		return nil
	}
	if o.MaxDelete >= 0 && p.Deletes > o.MaxDelete {
		return fmt.Errorf("%w: se borrarían %d entradas del destino y el límite es %d (ajustable con --max-delete)",
			ErrSafety, p.Deletes, o.MaxDelete)
	}
	if o.ConfirmMass && p.Deletes >= o.ConfirmOver {
		msg := fmt.Sprintf("Se borrarán %d entradas del destino.", p.Deletes)
		if o.Confirm == nil || !o.Confirm(msg) {
			return fmt.Errorf("%w: borrado masivo sin confirmar (usa --yes para omitir la pregunta)", ErrSafety)
		}
	}
	return nil
}

// ---------------------------------------------------------------- ejecución

// jobID identifica una transferencia de forma determinística: la misma
// (destino, tamaño, mtime) produce el mismo temporal, y por eso una corrida
// interrumpida se puede reanudar. Con Resume=false se le suma un nonce.
func jobID(dstPath string, size, mtime int64, resume bool) string {
	h := newHash()
	fmt.Fprintf(h, "%s\x00%d\x00%d", dstPath, size, mtime)
	if !resume {
		fmt.Fprintf(h, "\x00%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

type Engine struct {
	src, dst FS
	o        *Options
	sem      *byteSem
	lim      *limiter
	stats    *Stats
	prog     *Progress
}

func errChanged(a *Action) error {
	return fmt.Errorf("%w: %s cambió mientras se transfería (reintentá)", ErrVerify, a.Rel)
}

func (e *Engine) count(a *Action, sent, reused int64) {
	if a.Dst == nil || a.Replace {
		e.stats.NewFiles.Add(1)
	} else {
		e.stats.ModFiles.Add(1)
	}
	e.stats.SentBytes.Add(sent)
	e.stats.ReusedBytes.Add(reused)
}

func (e *Engine) transferSmall(a *Action, job string) error {
	size := int(a.Src.Size)
	var data []byte
	if size > 0 {
		e.lim.wait(size)
		d, err := e.src.ReadAt(a.SrcPath, 0, size)
		if err != nil {
			return err
		}
		if len(d) != size {
			return errChanged(a)
		}
		data = d
	}
	sum := sumBytes(data)
	w, err := e.dst.Begin(a.DstPath, job, a.Src.Size, false)
	if err != nil {
		return err
	}
	if size > 0 {
		if err := w.WriteAt(data, 0); err != nil {
			w.Abort()
			return err
		}
	}
	if err := w.Commit(sum, a.Src.Mode, a.Src.MTime); err != nil {
		return err
	}
	e.count(a, int64(size), 0)
	e.prog.Add(int64(size))
	return nil
}

func (e *Engine) transferLarge(a *Action, job string) error {
	bs := e.o.BlockSize
	size := a.Src.Size
	ss, err := e.src.Sigs(a.SrcPath, "", bs)
	if err != nil {
		return err
	}
	if !ss.Exists || ss.Size != size {
		return errChanged(a)
	}
	ds, err := e.dst.Sigs(a.DstPath, job, bs)
	if err != nil {
		return err
	}
	var changed []int
	var changedBytes int64
	for i := range ss.Blocks {
		if !ds.Exists || i >= len(ds.Blocks) || ds.Blocks[i] != ss.Blocks[i] {
			changed = append(changed, i)
			changedBytes += minInt64(int64(bs), size-int64(i)*int64(bs))
		}
	}

	// Mismo contenido que el archivo final: sólo metadatos.
	if len(changed) == 0 && ds.Exists && ds.Kind == "final" && ds.Size == size {
		if err := e.dst.SetMeta(a.DstPath, a.Src.Mode, a.Src.MTime); err != nil {
			return err
		}
		e.stats.MetaOnly.Add(1)
		e.stats.ReusedBytes.Add(size)
		e.prog.Add(size)
		return nil
	}

	e.prog.Add(size - changedBytes) // lo que ya está en destino cuenta como avance
	w, err := e.dst.Begin(a.DstPath, job, size, ds.Exists && ds.Kind == "final")
	if err != nil {
		return err
	}
	var (
		wg       sync.WaitGroup
		emu      sync.Mutex
		firstErr error
	)
	setErr := func(err error) {
		emu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		emu.Unlock()
	}
	failed := func() bool {
		emu.Lock()
		defer emu.Unlock()
		return firstErr != nil
	}
	for _, i := range changed {
		if failed() {
			break
		}
		off := int64(i) * int64(bs)
		n := int(minInt64(int64(bs), size-off))
		got := e.sem.acquire(int64(n))
		wg.Add(1)
		go func(off int64, n int, got int64) {
			defer wg.Done()
			defer e.sem.release(got)
			if failed() {
				return
			}
			e.lim.wait(n)
			data, err := e.src.ReadAt(a.SrcPath, off, n)
			if err == nil && len(data) != n {
				err = errChanged(a)
			}
			if err == nil {
				err = w.WriteAt(data, off)
			}
			if err != nil {
				setErr(err)
				return
			}
			e.prog.Add(int64(n))
		}(off, n, got)
	}
	wg.Wait()
	if firstErr != nil {
		w.Abort() // el temporal queda en disco: la próxima corrida reanuda desde ahí
		return firstErr
	}
	if err := w.Commit(ss.Hash, a.Src.Mode, a.Src.MTime); err != nil {
		return err
	}
	e.count(a, changedBytes, size-changedBytes)
	return nil
}

func (e *Engine) transferFile(a *Action) error {
	size := a.Src.Size
	if a.Replace && a.Dst != nil {
		if err := e.dst.Remove(a.DstPath, true); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if a.Rel == "." { // archivo suelto: asegurar el directorio padre
		if err := e.dst.Mkdir(path.Dir(a.DstPath), 0755); err != nil {
			return err
		}
	}
	job := jobID(a.DstPath, size, a.Src.MTime, e.o.Resume)
	var err error
	if size <= e.o.SmallFile {
		err = e.transferSmall(a, job)
	} else {
		err = e.transferLarge(a, job)
	}
	if err != nil {
		return err
	}
	e.stats.FileBytes.Add(size)
	e.prog.FileDone()
	return nil
}

func (e *Engine) doAction(a *Action) error {
	switch a.Kind {
	case ActLink:
		if a.Dst != nil {
			if err := e.dst.Remove(a.DstPath, true); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if err := e.dst.Symlink(a.Src.Link, a.DstPath); err != nil {
			return err
		}
		e.stats.Links.Add(1)
	case ActFile:
		if err := e.transferFile(a); err != nil {
			return err
		}
	}
	if e.o.Verbose {
		e.prog.Logf("  %s", displayName(a))
	}
	return nil
}

func displayName(a *Action) string {
	if a.Rel == "." {
		return path.Base(a.DstPath)
	}
	return a.Rel
}

// Execute aplica el plan: 1) directorios, 2) archivos y enlaces en paralelo
// con workers acotados, 3) permisos, 4) borrados (sólo si todo lo anterior salió bien).
func (e *Engine) Execute(plan *Plan) (*Stats, []Failure, error) {
	start := time.Now()
	e.prog.Start()
	defer func() {
		e.prog.Stop()
		e.stats.Elapsed = time.Since(start)
	}()

	var fails []Failure
	var fmu sync.Mutex
	addFail := func(a *Action, err error) {
		fmu.Lock()
		fails = append(fails, Failure{Rel: displayName(a), Err: err})
		fmu.Unlock()
		e.stats.Failed.Add(1)
	}

	var work, metas, dels []*Action
	for i := range plan.Actions {
		a := &plan.Actions[i]
		switch a.Kind {
		case ActMkdir:
			if a.Replace && a.Dst != nil {
				if err := e.dst.Remove(a.DstPath, true); err != nil && !errors.Is(err, fs.ErrNotExist) {
					addFail(a, err)
					continue
				}
			}
			if err := e.dst.Mkdir(a.DstPath, a.Src.Mode); err != nil {
				addFail(a, err)
				continue
			}
			e.stats.Dirs.Add(1)
		case ActFile, ActLink:
			work = append(work, a)
		case ActMeta:
			metas = append(metas, a)
		case ActDelete:
			dels = append(dels, a)
		}
	}

	jobs := make(chan *Action)
	var wg sync.WaitGroup
	var abort atomic.Bool
	workers := e.o.Workers
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				if abort.Load() {
					continue
				}
				if err := e.doAction(a); err != nil {
					addFail(a, err)
					if errors.Is(err, ErrConn) {
						abort.Store(true)
					}
				}
			}
		}()
	}
	for _, a := range work {
		jobs <- a
	}
	close(jobs)
	wg.Wait()
	if abort.Load() {
		return e.stats, fails, fmt.Errorf("%w: transferencia interrumpida", ErrConn)
	}

	for _, a := range metas {
		if err := e.dst.SetMeta(a.DstPath, a.Src.Mode, 0); err != nil {
			addFail(a, err)
		}
	}

	if len(dels) > 0 {
		if len(fails) > 0 {
			fmt.Fprintf(e.o.Err, "aviso: hubo errores; se omiten los %d borrados\n", len(dels))
		} else {
			for _, a := range dels {
				if err := e.dst.Remove(a.DstPath, a.Dst.Type == TDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
					addFail(a, err)
					continue
				}
				e.stats.Deleted.Add(1)
				if e.o.Verbose {
					e.prog.Logf("  - %s", a.Rel)
				}
			}
		}
	}

	if len(fails) > 0 {
		return e.stats, fails, ErrPartial
	}
	return e.stats, nil, nil
}

// Run planifica y (salvo dryRun) ejecuta una sincronización origen -> destino.
// wire, si no es nil, informa los bytes acumulados en el cable.
func Run(src FS, srcPath string, dst FS, dstPath string, o *Options, dryRun bool, wire func() int64) (*Result, error) {
	plan, err := BuildPlan(src, srcPath, dst, dstPath, o)
	if err != nil {
		return nil, err
	}
	res := &Result{Plan: plan}
	if dryRun {
		PrintPlan(o.Out, plan, o.Delete)
		if !o.Quiet {
			fmt.Fprintln(o.Err, plan.Summary())
		}
		return res, nil
	}
	if err := checkSafety(plan, o); err != nil {
		return res, err
	}
	if !plan.HasChanges() {
		if !o.Quiet {
			fmt.Fprintln(o.Err, "Todo al día: nada que transferir.")
		}
		return res, nil
	}
	var w0 int64
	if wire != nil {
		w0 = wire()
	}
	eng := &Engine{
		src: src, dst: dst, o: o,
		sem:   newByteSem(o.MaxInflight),
		lim:   newLimiter(o.BWLimit),
		stats: &Stats{},
		prog:  NewProgress(o.Err, o.TTY, o.Quiet, int64(plan.Files), plan.Bytes, o.Verbose),
	}
	stats, fails, err := eng.Execute(plan)
	res.Stats, res.Failures = stats, fails
	if !o.Quiet {
		var wb int64
		if wire != nil {
			wb = wire() - w0
		}
		printSummary(o.Err, plan, stats, wb)
	}
	for _, f := range fails {
		fmt.Fprintf(o.Err, "error: %s: %v\n", f.Rel, f.Err)
	}
	return res, err
}
