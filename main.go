package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
)

// version se fija en el build con -ldflags "-X main.version=...".
var version = "0.1.0-demo"

// Códigos de salida estables (para scripts).
const (
	exitOK      = 0
	exitError   = 1  // error genérico (E/S, ruta inexistente...)
	exitUsage   = 2  // uso incorrecto
	exitConn    = 3  // conexión / ssh / agente
	exitVerify  = 4  // integridad: hash distinto o archivo cambió durante la copia
	exitSafety  = 5  // abortado por salvaguarda (límite de borrados, sin confirmación)
	exitPartial = 6  // terminó, pero algunos archivos fallaron
	exitDiffs   = 10 // sólo `vx diff --exit-code`: hay diferencias
)

var errDiffs = errors.New("hay diferencias")

func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errDiffs):
		return exitDiffs
	case errors.Is(err, ErrUsage):
		return exitUsage
	case errors.Is(err, ErrConn):
		return exitConn
	case errors.Is(err, ErrVerify):
		return exitVerify
	case errors.Is(err, ErrSafety):
		return exitSafety
	case errors.Is(err, ErrPartial):
		return exitPartial
	}
	return exitError
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, in io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "put", "get", "sync", "diff":
		err = cmdTransfer(cmd, rest, in, stdout, stderr)
	case "connect":
		err = cmdConnect(rest, in, stdout, stderr)
	case "config":
		err = cmdConfig(rest, stdout)
	case "agent":
		err = cmdAgent(rest, in, stdout)
	case "install-remote":
		err = cmdInstallRemote(rest)
	case "version", "--version", "-V":
		fmt.Fprintf(stdout, "vextra %s (%s/%s, %s, hash=%s, codec=%s)\n",
			version, runtime.GOOS, runtime.GOARCH, runtime.Version(), hashName, codecName)
	case "help", "-h", "--help":
		usage(stdout)
	default:
		err = fmt.Errorf("%w: comando desconocido %q (probá: vx help)", ErrUsage, cmd)
	}
	if err != nil && !errors.Is(err, errDiffs) {
		fmt.Fprintf(stderr, "vx: %v\n", err)
	}
	return exitCode(err)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Vextra — transferencia y sincronización de archivos sobre SSH  (binario: vextra; alias: vx)

Uso:
  vx connect [usuario@]host            shell interactivo (ls cd get put sync diff ...)
  vx put  ORIGEN [usuario@]host:DEST   copia local -> remoto
  vx get  [usuario@]host:ORIGEN DEST   copia remoto -> local
  vx sync ORIGEN DESTINO               espejo unidireccional (en cualquier dirección, o local-local)
  vx diff ORIGEN DESTINO               muestra qué cambiaría, sin tocar nada
  vx config show                       configuración efectiva
  vx install-remote [usuario@]host     copia este binario a ~/.local/bin/vextra del remoto
  vx version

Opciones (put/get/sync/diff):
  -n, --dry-run        no modifica nada; muestra el plan
      --delete         (sync/diff) borra en destino lo que no está en origen
      --max-delete N   aborta si hay más de N borrados (0 = ninguno, -1 = sin límite)
  -y, --yes            no pide confirmación ante borrados masivos
      --workers N      archivos en paralelo (por defecto: auto)
      --block-size S   tamaño de bloque del delta (por defecto 1MiB)
      --bwlimit S      límite de ancho de banda, p. ej. 20MiB (por segundo)
      --no-compress    no comprime
      --no-resume      no reutiliza temporales de corridas interrumpidas
      --ssh CMD        comando ssh a usar, p. ej. "ssh -p 2222 -i ~/.ssh/k"  (o $VX_SSH)
      --remote-root D  el agente remoto sólo puede tocar rutas dentro de D
      --config FILE    archivo de configuración (o $VX_CONFIG)
  -v, --verbose        lista cada archivo procesado
  -q, --quiet          sin progreso ni resumen
      --exit-code      (diff) sale con código 10 si hay diferencias

Códigos de salida: 0 ok · 1 error · 2 uso · 3 conexión · 4 integridad · 5 salvaguarda · 6 parcial · 10 diff
Rutas remotas: [usuario@]host:ruta   (relativas al home del usuario remoto)
`)
}

// ------------------------------------------------------------------ flags

type xferFlags struct {
	dry, del, yes, verbose, quiet, noCompress, noResume, exitCode bool
	maxDelete, workers                                            int
	blockSize, bwlimit, ssh, remoteRoot, configPath               string
	set                                                           map[string]bool
}

func (x *xferFlags) register(fl *flag.FlagSet, cmd string) {
	fl.BoolVar(&x.dry, "n", false, "")
	fl.BoolVar(&x.dry, "dry-run", false, "")
	if cmd == "sync" || cmd == "diff" {
		fl.BoolVar(&x.del, "delete", false, "")
		fl.IntVar(&x.maxDelete, "max-delete", 0, "")
	}
	if cmd == "diff" {
		fl.BoolVar(&x.exitCode, "exit-code", false, "")
	}
	fl.BoolVar(&x.yes, "y", false, "")
	fl.BoolVar(&x.yes, "yes", false, "")
	fl.IntVar(&x.workers, "workers", 0, "")
	fl.StringVar(&x.blockSize, "block-size", "", "")
	fl.StringVar(&x.bwlimit, "bwlimit", "", "")
	fl.BoolVar(&x.noCompress, "no-compress", false, "")
	fl.BoolVar(&x.noResume, "no-resume", false, "")
	fl.StringVar(&x.ssh, "ssh", "", "")
	fl.StringVar(&x.remoteRoot, "remote-root", "", "")
	fl.StringVar(&x.configPath, "config", "", "")
	fl.BoolVar(&x.verbose, "v", false, "")
	fl.BoolVar(&x.verbose, "verbose", false, "")
	fl.BoolVar(&x.quiet, "q", false, "")
	fl.BoolVar(&x.quiet, "quiet", false, "")
}

// parseFlags admite flags antes y después de los argumentos posicionales
// (vx sync a b --delete), cosa que el paquete flag no hace por sí solo.
func parseFlags(fl *flag.FlagSet, args []string, x *xferFlags) ([]string, error) {
	var pos []string
	for {
		if err := fl.Parse(args); err != nil {
			return nil, err
		}
		args = fl.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if x != nil {
		x.set = map[string]bool{}
		fl.Visit(func(f *flag.Flag) { x.set[f.Name] = true })
	}
	return pos, nil
}

func newFlagSet(name string) *flag.FlagSet {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	return fl
}

func buildOptions(cfg *Config, x *xferFlags, confirm func(string) bool, out, errw io.Writer) (*Options, error) {
	o := &Options{
		Delete: x.del, MaxDelete: cfg.MaxDelete, ConfirmMass: cfg.ConfirmMass, ConfirmOver: cfg.ConfirmOver,
		Confirm: confirm, Workers: cfg.EffectiveWorkers(), MaxInflight: cfg.MaxInflight,
		BlockSize: cfg.BlockSize, SmallFile: cfg.SmallFile,
		Resume: cfg.Resume && !x.noResume, Verbose: x.verbose, Quiet: x.quiet,
		TTY: isTTY(os.Stderr), Out: out, Err: errw,
	}
	if x.set["max-delete"] {
		o.MaxDelete = x.maxDelete
	}
	if x.set["workers"] {
		if x.workers < 1 {
			return nil, fmt.Errorf("%w: --workers debe ser >= 1", ErrUsage)
		}
		o.Workers = x.workers
	}
	if x.blockSize != "" {
		v, err := parseSize(x.blockSize)
		if err != nil || v < 4<<10 || v > 16<<20 {
			return nil, fmt.Errorf("%w: --block-size debe estar entre 4KiB y 16MiB", ErrUsage)
		}
		o.BlockSize = int(v)
	}
	if x.bwlimit != "" {
		v, err := parseSize(x.bwlimit)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUsage, err)
		}
		o.BWLimit = v
	}
	return o, nil
}

func confirmFunc(yes bool, in io.Reader, w io.Writer) func(string) bool {
	if yes {
		return func(string) bool { return true }
	}
	f, ok := in.(*os.File)
	if !ok || !isTTY(f) {
		return nil
	}
	r := bufio.NewReader(in)
	return func(msg string) bool {
		fmt.Fprintf(w, "%s ¿Continuar? [s/N]: ", msg)
		line, _ := r.ReadString('\n')
		return isYes(line)
	}
}

// ------------------------------------------------------------------ comandos

func cmdTransfer(cmd string, args []string, in io.Reader, stdout, stderr io.Writer) error {
	fl := newFlagSet(cmd)
	x := &xferFlags{}
	x.register(fl, cmd)
	pos, err := parseFlags(fl, args, x)
	if errors.Is(err, flag.ErrHelp) {
		usage(stdout)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if len(pos) != 2 {
		return fmt.Errorf("%w: uso: vx %s ORIGEN DESTINO", ErrUsage, cmd)
	}
	srcEP, dstEP := ParseEndpoint(pos[0]), ParseEndpoint(pos[1])
	switch {
	case srcEP.Remote() && dstEP.Remote():
		return fmt.Errorf("%w: remoto -> remoto no está soportado en V1", ErrUsage)
	case cmd == "put" && srcEP.Remote():
		return fmt.Errorf("%w: put copia local -> remoto (para bajar usá: vx get)", ErrUsage)
	case cmd == "get" && dstEP.Remote():
		return fmt.Errorf("%w: get copia remoto -> local (para subir usá: vx put)", ErrUsage)
	}
	cfg, err := LoadConfig(x.configPath)
	if err != nil {
		return err
	}
	o, err := buildOptions(cfg, x, confirmFunc(x.yes, in, stderr), stdout, stderr)
	if err != nil {
		return err
	}
	sess := NewSession(cfg, x.ssh, x.remoteRoot, cfg.Compression != "off" && !x.noCompress)
	defer sess.Close()
	srcFS, err := sess.FS(srcEP)
	if err != nil {
		return err
	}
	dstFS, err := sess.FS(dstEP)
	if err != nil {
		return err
	}
	res, err := Run(srcFS, srcEP.Path, dstFS, dstEP.Path, o, cmd == "diff" || x.dry, sess.Wire)
	if err != nil {
		return err
	}
	if cmd == "diff" && x.exitCode && res != nil && res.Plan != nil &&
		(res.Plan.HasChanges() || len(res.Plan.Extra) > 0) {
		return errDiffs
	}
	return nil
}

func cmdConnect(args []string, in io.Reader, stdout, stderr io.Writer) error {
	fl := newFlagSet("connect")
	x := &xferFlags{}
	fl.StringVar(&x.ssh, "ssh", "", "")
	fl.StringVar(&x.remoteRoot, "remote-root", "", "")
	fl.StringVar(&x.configPath, "config", "", "")
	fl.BoolVar(&x.noCompress, "no-compress", false, "")
	pos, err := parseFlags(fl, args, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: uso: vx connect [usuario@]host", ErrUsage)
	}
	cfg, err := LoadConfig(x.configPath)
	if err != nil {
		return err
	}
	sess := NewSession(cfg, x.ssh, x.remoteRoot, cfg.Compression != "off" && !x.noCompress)
	defer sess.Close()
	return RunShell(sess, pos[0], in, stdout, stderr)
}

func cmdConfig(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "show" {
		return fmt.Errorf("%w: uso: vx config show", ErrUsage)
	}
	fl := newFlagSet("config")
	x := &xferFlags{}
	fl.StringVar(&x.configPath, "config", "", "")
	if _, err := parseFlags(fl, args[1:], nil); err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	cfg, err := LoadConfig(x.configPath)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, cfg.String())
	return nil
}

// cmdAgent es el lado remoto: lo lanza el cliente vía ssh. stdout es el
// canal del protocolo, por eso NADA más puede escribir ahí.
func cmdAgent(args []string, in io.Reader, stdout io.Writer) error {
	fl := newFlagSet("agent")
	var root string
	fl.StringVar(&root, "root", "", "")
	if _, err := parseFlags(fl, args, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	home, _ := os.UserHomeDir()
	base := home
	if root != "" {
		base = root
	}
	l := NewLocalFS(root, base)
	if _, err := io.WriteString(stdout, agentMagic); err != nil {
		return err
	}
	return Serve(bufio.NewReaderSize(in, 256<<10), stdout, l)
}

func cmdInstallRemote(args []string) error {
	fl := newFlagSet("install-remote")
	x := &xferFlags{}
	fl.StringVar(&x.ssh, "ssh", "", "")
	fl.StringVar(&x.configPath, "config", "", "")
	pos, err := parseFlags(fl, args, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUsage, err)
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: uso: vx install-remote [usuario@]host", ErrUsage)
	}
	cfg, err := LoadConfig(x.configPath)
	if err != nil {
		return err
	}
	return installRemote(NewSession(cfg, x.ssh, "", false).SSH, pos[0])
}
