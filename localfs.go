package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// LocalFS implementa FS sobre el filesystem del sistema. Lo usa el cliente
// para el lado local y el agente (`vextra agent`) para el lado remoto.
type LocalFS struct {
	root     string // "" = sin confinamiento; si no, todo path debe quedar dentro
	realRoot string
	base     string // contra qué se resuelven rutas relativas ("" = cwd del proceso)
}

func NewLocalFS(root, base string) *LocalFS {
	l := &LocalFS{base: base}
	if root != "" && root != "/" {
		l.root = path.Clean(root)
		if r, err := filepath.EvalSymlinks(l.root); err == nil {
			l.realRoot = r
		} else {
			l.realRoot = l.root
		}
	}
	return l
}

// resolve normaliza p y, si hay raíz autorizada, rechaza traversal (../) y
// symlinks de directorios intermedios que escapen de ella.
func (l *LocalFS) resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !path.IsAbs(p) && l.base != "" {
		p = path.Join(l.base, p)
	}
	p = path.Clean(p)
	if l.root == "" {
		return p, nil
	}
	if !path.IsAbs(p) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, p)
	}
	if p != l.root && !strings.HasPrefix(p, l.root+"/") {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, p)
	}
	if p != l.root {
		if err := l.checkEscape(path.Dir(p)); err != nil {
			return "", err
		}
	}
	return p, nil
}

// checkEscape resuelve los symlinks del ancestro existente más profundo de dir
// y verifica que siga dentro de la raíz.
func (l *LocalFS) checkEscape(dir string) error {
	cur := dir
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		}
		parent := path.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return nil
	}
	if real == l.realRoot || strings.HasPrefix(real, l.realRoot+"/") {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrOutsideRoot, dir)
}

func (l *LocalFS) Home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/"
}

func (l *LocalFS) Close() error { return nil }

func isTempName(name string) bool { return strings.Contains(name, tmpMarker) }

func entryFromInfo(rel, full string, fi os.FileInfo) (*Entry, bool) {
	e := &Entry{Path: rel, Mode: uint32(fi.Mode().Perm()), MTime: fi.ModTime().UnixNano()}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		e.Type = TLink
		if t, err := os.Readlink(full); err == nil {
			e.Link = t
		}
	case fi.IsDir():
		e.Type = TDir
	case fi.Mode().IsRegular():
		e.Type = TFile
		e.Size = fi.Size()
	default:
		return nil, false // sockets, dispositivos, fifos: se ignoran
	}
	return e, true
}

func (l *LocalFS) Stat(p string, follow bool) (*Entry, error) {
	rp, err := l.resolve(p)
	if err != nil {
		return nil, err
	}
	var fi os.FileInfo
	if follow {
		fi, err = os.Stat(rp)
	} else {
		fi, err = os.Lstat(rp)
	}
	if err != nil {
		return nil, err
	}
	e, ok := entryFromInfo(".", rp, fi)
	if !ok {
		return nil, fmt.Errorf("%s: tipo de archivo no soportado", p)
	}
	return e, nil
}

func (l *LocalFS) List(p string, recursive bool) ([]Entry, error) {
	rp, err := l.resolve(p)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(rp) // sigue un symlink raíz
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		fi, err := os.Lstat(rp)
		if err != nil {
			return nil, err
		}
		e, ok := entryFromInfo(".", rp, fi)
		if !ok {
			return nil, fmt.Errorf("%s: tipo de archivo no soportado", p)
		}
		return []Entry{*e}, nil
	}
	real, err := filepath.EvalSymlinks(rp)
	if err != nil {
		return nil, err
	}
	var out []Entry
	if !recursive {
		ents, err := os.ReadDir(real)
		if err != nil {
			return nil, err
		}
		for _, d := range ents {
			if isTempName(d.Name()) {
				continue
			}
			fi, err := d.Info()
			if err != nil {
				continue
			}
			if e, ok := entryFromInfo(d.Name(), filepath.Join(real, d.Name()), fi); ok {
				out = append(out, *e)
			}
		}
		return out, nil
	}
	// Recursivo: cualquier error de lectura ABORTA (un directorio ilegible no
	// puede confundirse con "vacío": con --delete eso borraría de más).
	err = filepath.WalkDir(real, func(fp string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if fp == real {
			return nil
		}
		if isTempName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(real, fp)
		if rerr != nil {
			return rerr
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if e, ok := entryFromInfo(filepath.ToSlash(rel), fp, fi); ok {
			out = append(out, *e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (l *LocalFS) Mkdir(p string, mode uint32) error {
	rp, err := l.resolve(p)
	if err != nil {
		return err
	}
	return os.MkdirAll(rp, os.FileMode(mode&0777)|0700)
}

func (l *LocalFS) Remove(p string, recursive bool) error {
	rp, err := l.resolve(p)
	if err != nil {
		return err
	}
	if rp == "/" || rp == "." || (l.root != "" && rp == l.root) {
		return errors.New("se rechaza borrar la raíz")
	}
	if recursive {
		return os.RemoveAll(rp)
	}
	return os.Remove(rp)
}

func (l *LocalFS) Rename(from, to string) error {
	a, err := l.resolve(from)
	if err != nil {
		return err
	}
	b, err := l.resolve(to)
	if err != nil {
		return err
	}
	return os.Rename(a, b)
}

func (l *LocalFS) Symlink(target, p string) error {
	rp, err := l.resolve(p)
	if err != nil {
		return err
	}
	return os.Symlink(target, rp)
}

func (l *LocalFS) SetMeta(p string, mode uint32, mtime int64) error {
	rp, err := l.resolve(p)
	if err != nil {
		return err
	}
	if err := os.Chmod(rp, os.FileMode(mode&0777)); err != nil {
		return err
	}
	if mtime != 0 {
		t := time.Unix(0, mtime)
		return os.Chtimes(rp, t, t)
	}
	return nil
}

// openRegular abre sólo archivos regulares y NO sigue un symlink final.
func openRegular(p string) (*os.File, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: no es un archivo regular", p)
	}
	return os.Open(p)
}

func (l *LocalFS) Sigs(p, job string, bs int) (*Sigs, error) {
	rp, err := l.resolve(p)
	if err != nil {
		return nil, err
	}
	target, kind := rp, "final"
	if job != "" {
		tmp := rp + tmpMarker + job
		if fi, err := os.Lstat(tmp); err == nil && fi.Mode().IsRegular() {
			target, kind = tmp, "tmp"
		}
	}
	f, err := openRegular(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Sigs{}, nil
		}
		return nil, err
	}
	defer f.Close()
	return computeSigs(f, bs, kind)
}

func computeSigs(r io.Reader, bs int, kind string) (*Sigs, error) {
	if bs <= 0 {
		return nil, errors.New("tamaño de bloque inválido")
	}
	s := &Sigs{Exists: true, Kind: kind, BS: bs}
	full := newHash()
	buf := make([]byte, bs)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			s.Size += int64(n)
			full.Write(buf[:n])
			s.Blocks = append(s.Blocks, sumBytes(buf[:n]))
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	copy(s.Hash[:], full.Sum(nil))
	return s, nil
}

func (l *LocalFS) ReadAt(p string, off int64, n int) ([]byte, error) {
	if n < 0 {
		return nil, errors.New("largo negativo")
	}
	rp, err := l.resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := openRegular(rp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	m, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:m], nil
}

// removeStaleTmp borra temporales de OTROS jobs para el mismo destino.
func removeStaleTmp(dst, keep string) {
	dir, base := path.Split(dst)
	if dir == "" {
		dir = "."
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := base + tmpMarker
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, prefix) && path.Join(dir, n) != keep {
			os.Remove(path.Join(dir, n))
		}
	}
}

func copyBase(src string, dst *os.File) error {
	in, err := openRegular(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()
	_, err = io.Copy(dst, in)
	return err
}

func (l *LocalFS) Begin(p, job string, size int64, useBase bool) (Writer, error) {
	rp, err := l.resolve(p)
	if err != nil {
		return nil, err
	}
	tmp := rp + tmpMarker + job
	removeStaleTmp(rp, tmp)
	var f *os.File
	if fi, serr := os.Lstat(tmp); serr == nil && fi.Mode().IsRegular() {
		// Reanudación: el temporal de este job ya existe y se reutiliza tal cual.
		f, err = os.OpenFile(tmp, os.O_RDWR, 0600)
	} else {
		f, err = os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil && useBase {
			if cerr := copyBase(rp, f); cerr != nil {
				f.Close()
				os.Remove(tmp)
				return nil, cerr
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return &localWriter{f: f, tmp: tmp, dst: rp, size: size}, nil
}

type localWriter struct {
	f    *os.File
	tmp  string
	dst  string
	size int64
}

func (w *localWriter) WriteAt(p []byte, off int64) error {
	_, err := w.f.WriteAt(p, off)
	return err
}

// Abort cierra el descriptor pero deja el temporal en disco para reanudar.
func (w *localWriter) Abort() { w.f.Close() }

func (w *localWriter) Commit(sum Digest, mode uint32, mtime int64) error {
	defer w.f.Close()
	if err := w.f.Truncate(w.size); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	// Verificación de integridad del archivo completo ANTES de publicarlo.
	h := newHash()
	if _, err := io.Copy(h, io.NewSectionReader(w.f, 0, w.size)); err != nil {
		return err
	}
	var got Digest
	copy(got[:], h.Sum(nil))
	if got != sum {
		os.Remove(w.tmp)
		return fmt.Errorf("%w: %s (esperado %s…, obtenido %s…)", ErrVerify, w.dst, sum.String()[:12], got.String()[:12])
	}
	if err := os.Chmod(w.tmp, os.FileMode(mode&0777)); err != nil {
		return err
	}
	t := time.Unix(0, mtime)
	if err := os.Chtimes(w.tmp, t, t); err != nil {
		return err
	}
	if err := os.Rename(w.tmp, w.dst); err != nil {
		return err
	}
	syncDir(path.Dir(w.dst))
	// Verificación post-commit: el destino final existe y tiene el tamaño esperado.
	fi, err := os.Lstat(w.dst)
	if err != nil {
		return err
	}
	if fi.Size() != w.size {
		return fmt.Errorf("%w: %s tiene %d bytes, se esperaban %d", ErrVerify, w.dst, fi.Size(), w.size)
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
