package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Shell es la sesión interactiva: comandos remotos (ls, cd, pwd, mkdir, rm,
// mv), sus gemelos locales con prefijo "l", y get/put/sync/diff.
type Shell struct {
	sess *Session
	rfs  FS
	host string
	rcwd string
	in   *bufio.Reader
	out  io.Writer
	errw io.Writer
}

// scope agrupa un FS con su forma de resolver rutas (local o remoto).
type scope struct {
	fs      FS
	resolve func(string) string
}

func RunShell(sess *Session, host string, in io.Reader, out, errw io.Writer) error {
	rfs, err := sess.FS(Endpoint{Host: host})
	if err != nil {
		return err
	}
	sh := &Shell{sess: sess, rfs: rfs, host: host, rcwd: rfs.Home(), in: bufio.NewReader(in), out: out, errw: errw}
	fmt.Fprintf(out, "Conectado a %s (cwd remoto: %s). Escribí 'help' para ver los comandos.\n", host, sh.rcwd)
	for {
		fmt.Fprint(out, sh.prompt())
		line, err := sh.in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(out)
			return nil
		}
		quit, cerr := sh.exec(strings.TrimSpace(line))
		if cerr != nil {
			fmt.Fprintf(errw, "error: %v\n", cerr)
			if errors.Is(cerr, ErrConn) {
				return cerr
			}
		}
		if quit {
			return nil
		}
	}
}

func (s *Shell) prompt() string {
	cwd, _ := os.Getwd()
	if h, err := os.UserHomeDir(); err == nil && (cwd == h || strings.HasPrefix(cwd, h+"/")) {
		cwd = "~" + cwd[len(h):]
	}
	return fmt.Sprintf("vx [local:%s] [remote:%s]> ", cwd, s.rcwd)
}

func (s *Shell) rpath(p string) string {
	if p == "" {
		return s.rcwd
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = s.rfs.Home() + p[1:]
	}
	if !path.IsAbs(p) {
		p = path.Join(s.rcwd, p)
	}
	return path.Clean(p)
}

func (s *Shell) scopeOf(remote bool) scope {
	if remote {
		return scope{fs: s.rfs, resolve: s.rpath}
	}
	return scope{fs: s.sess.local, resolve: func(p string) string {
		if p == "" {
			return "."
		}
		return expandTilde(p)
	}}
}

func (s *Shell) confirm(msg string) bool {
	fmt.Fprintf(s.out, "%s [s/N]: ", msg)
	line, _ := s.in.ReadString('\n')
	return isYes(line)
}

func (s *Shell) exec(line string) (bool, error) {
	if line == "" || strings.HasPrefix(line, "#") {
		return false, nil
	}
	args, err := splitArgs(line)
	if err != nil {
		return false, err
	}
	if len(args) == 0 {
		return false, nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "exit", "quit", "bye":
		return true, nil
	case "help", "?":
		s.help()
	case "pwd":
		fmt.Fprintln(s.out, s.rcwd)
	case "lpwd":
		var cwd string
		if cwd, err = os.Getwd(); err == nil {
			fmt.Fprintln(s.out, cwd)
		}
	case "cd":
		err = s.cd(rest)
	case "lcd":
		target := "~"
		if len(rest) > 0 {
			target = rest[0]
		}
		err = os.Chdir(expandTilde(target))
	case "ls":
		err = s.ls(s.scopeOf(true), rest)
	case "lls":
		err = s.ls(s.scopeOf(false), rest)
	case "mkdir", "rm", "mv":
		err = s.fileOp(cmd, true, rest)
	case "lmkdir", "lrm", "lmv":
		err = s.fileOp(cmd[1:], false, rest)
	case "get", "put", "sync", "diff":
		err = s.transfer(cmd, rest)
	default:
		err = fmt.Errorf("comando desconocido: %s (help muestra la lista)", cmd)
	}
	return false, err
}

func (s *Shell) help() {
	fmt.Fprint(s.out, `Remoto:  ls [-l] [ruta] · cd [ruta] · pwd · mkdir ruta · rm [-r] ruta · mv origen destino
Local:   lls [-l] [ruta] · lcd [ruta] · lpwd · lmkdir ruta · lrm [-r] ruta · lmv origen destino
Copia:   put [opc] LOCAL [REMOTO]      sube (por defecto a <cwd remoto>/<nombre>)
         get [opc] REMOTO [LOCAL]      baja (por defecto a <cwd local>/<nombre>)
         sync [opc] LOCAL [REMOTO]     espejo local -> remoto      (--pull: remoto -> local)
         diff [opc] LOCAL [REMOTO]     muestra qué cambiaría        (--pull: remoto -> local)
Opciones de copia: -n/--dry-run · --delete (sync/diff) · --max-delete N · -y · -v · --workers N · --bwlimit S
Sesión:  help · exit
`)
}

func (s *Shell) cd(args []string) error {
	target := s.rfs.Home()
	if len(args) > 0 {
		target = s.rpath(args[0])
	}
	e, err := s.rfs.Stat(target, true)
	if err != nil {
		return err
	}
	if e.Type != TDir {
		return fmt.Errorf("%s no es un directorio", target)
	}
	s.rcwd = target
	return nil
}

func modeString(e *Entry) string {
	t := "-"
	switch e.Type {
	case TDir:
		t = "d"
	case TLink:
		t = "l"
	}
	return t + os.FileMode(e.Mode&0777).String()[1:]
}

func (s *Shell) ls(sc scope, args []string) error {
	long := false
	target := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			if strings.Contains(a, "l") {
				long = true
			}
		} else if target == "" {
			target = a
		}
	}
	rp := sc.resolve(target)
	e, err := sc.fs.Stat(rp, true)
	if err != nil {
		return err
	}
	var ents []Entry
	if e.Type == TDir {
		if ents, err = sc.fs.List(rp, false); err != nil {
			return err
		}
	} else {
		e.Path = path.Base(rp)
		ents = []Entry{*e}
	}
	if long {
		for i := range ents {
			x := &ents[i]
			name := x.Path
			if x.Type == TDir {
				name += "/"
			} else if x.Type == TLink {
				name += " -> " + x.Link
			}
			fmt.Fprintf(s.out, "%s %10s  %s  %s\n", modeString(x), humanBytes(x.Size),
				time.Unix(0, x.MTime).Format("2006-01-02 15:04"), name)
		}
		return nil
	}
	names := make([]string, 0, len(ents))
	for i := range ents {
		n := ents[i].Path
		if ents[i].Type == TDir {
			n += "/"
		}
		names = append(names, n)
	}
	printColumns(s.out, names)
	return nil
}

func printColumns(w io.Writer, names []string) {
	if len(names) == 0 {
		return
	}
	maxw := 0
	for _, n := range names {
		if l := utf8.RuneCountInString(n); l > maxw {
			maxw = l
		}
	}
	colw := maxw + 2
	cols := 80 / colw
	if cols < 1 {
		cols = 1
	}
	rows := (len(names) + cols - 1) / cols
	for r := 0; r < rows; r++ {
		var line strings.Builder
		for c := 0; c < cols; c++ {
			i := c*rows + r
			if i >= len(names) {
				break
			}
			line.WriteString(names[i])
			if (c+1)*rows+r < len(names) {
				line.WriteString(strings.Repeat(" ", colw-utf8.RuneCountInString(names[i])))
			}
		}
		fmt.Fprintln(w, line.String())
	}
}

func (s *Shell) fileOp(op string, remote bool, args []string) error {
	sc := s.scopeOf(remote)
	switch op {
	case "mkdir":
		if len(args) != 1 {
			return errors.New("uso: mkdir RUTA")
		}
		return sc.fs.Mkdir(sc.resolve(args[0]), 0755)
	case "rm":
		recursive, force := false, false
		var targets []string
		for _, a := range args {
			switch a {
			case "-r", "-R", "-rf", "-fr":
				recursive = true
				force = force || a == "-rf" || a == "-fr"
			case "-f":
				force = true
			default:
				targets = append(targets, a)
			}
		}
		if len(targets) == 0 {
			return errors.New("uso: rm [-r] [-f] RUTA...")
		}
		for _, t := range targets {
			p := sc.resolve(t)
			e, err := sc.fs.Stat(p, false)
			if err != nil {
				return err
			}
			if e.Type == TDir {
				if !recursive {
					return fmt.Errorf("%s es un directorio (usá -r)", t)
				}
				if !force && !s.confirm(fmt.Sprintf("¿Borrar recursivamente %s?", p)) {
					return errors.New("cancelado")
				}
			}
			if err := sc.fs.Remove(p, e.Type == TDir); err != nil {
				return err
			}
		}
		return nil
	case "mv":
		if len(args) != 2 {
			return errors.New("uso: mv ORIGEN DESTINO")
		}
		from, to := sc.resolve(args[0]), sc.resolve(args[1])
		if e, err := sc.fs.Stat(to, true); err == nil && e.Type == TDir {
			to = path.Join(to, path.Base(from))
		}
		return sc.fs.Rename(from, to)
	}
	return fmt.Errorf("operación desconocida: %s", op)
}

func (s *Shell) transfer(cmd string, args []string) error {
	fl := newFlagSet(cmd)
	x := &xferFlags{}
	x.register(fl, cmd)
	var pull bool
	if cmd == "sync" || cmd == "diff" {
		fl.BoolVar(&pull, "pull", false, "")
	}
	pos, err := parseFlags(fl, args, x)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("uso: %s [opciones] ORIGEN [DESTINO]", cmd)
	}
	if cmd == "get" {
		pull = true
	}
	local, remote := s.scopeOf(false), s.scopeOf(true)
	var srcSc, dstSc scope
	if pull {
		srcSc, dstSc = remote, local
	} else {
		srcSc, dstSc = local, remote
	}
	srcArg := pos[0]
	dstArg := ""
	if len(pos) == 2 {
		dstArg = pos[1]
	} else {
		dstArg = path.Base(srcArg)
	}
	srcPath, dstPath := srcSc.resolve(srcArg), dstSc.resolve(dstArg)

	o, err := buildOptions(s.sess.Cfg, x, func(msg string) bool {
		if x.yes {
			return true
		}
		return s.confirm(msg + " ¿Continuar?")
	}, s.out, s.errw)
	if err != nil {
		return err
	}
	_, err = Run(srcSc.fs, srcPath, dstSc.fs, dstPath, o, cmd == "diff" || x.dry, s.sess.Wire)
	return err
}
