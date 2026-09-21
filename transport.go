package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// Endpoint es un extremo de una transferencia: local (Host == "") o
// remoto ([usuario@]host:ruta).
type Endpoint struct {
	Host string
	Path string
}

func (e Endpoint) Remote() bool { return e.Host != "" }

// ParseEndpoint interpreta "host:ruta" como remoto sólo si lo que precede a
// los primeros ":" no contiene "/" (así "./a:b" y "/tmp/a:b" siguen siendo locales).
func ParseEndpoint(s string) Endpoint {
	if i := strings.Index(s, ":"); i > 0 && !strings.Contains(s[:i], "/") {
		p := s[i+1:]
		if p == "" {
			p = "."
		}
		return Endpoint{Host: s[:i], Path: p}
	}
	return Endpoint{Path: s}
}

// Session administra las conexiones de un comando: una por host remoto,
// abiertas recién cuando hacen falta.
type Session struct {
	Cfg        *Config
	SSH        []string
	RemoteRoot string
	Compress   bool
	remotes    map[string]*RemoteFS
	local      *LocalFS
}

// NewSession resuelve el comando ssh base con esta precedencia: flag --ssh,
// variable VX_SSH, clave `ssh` de la configuración, "ssh".
func NewSession(cfg *Config, sshFlag, remoteRoot string, compress bool) *Session {
	cmd := sshFlag
	if cmd == "" {
		cmd = os.Getenv("VX_SSH")
	}
	if cmd == "" {
		cmd = cfg.SSH
	}
	argv := strings.Fields(cmd)
	if len(argv) == 0 {
		argv = []string{"ssh"}
	}
	for i := range argv {
		argv[i] = expandTilde(argv[i]) // exec no pasa por un shell: expandimos ~/ nosotros
	}
	return &Session{
		Cfg: cfg, SSH: argv, RemoteRoot: remoteRoot, Compress: compress,
		remotes: map[string]*RemoteFS{}, local: NewLocalFS("", ""),
	}
}

// AddSSHOptions inserta opts justo después del ejecutable ssh. OpenSSH usa el
// primer valor que ve de cada opción, así que lo agregado acá gana sobre el
// comando base ($VX_SSH, configuración): lo explícito en la línea de comandos
// nunca queda pisado por un valor por defecto.
func (s *Session) AddSSHOptions(opts ...string) {
	if len(opts) == 0 {
		return
	}
	argv := make([]string, 0, len(s.SSH)+len(opts))
	argv = append(argv, s.SSH[0])
	argv = append(argv, opts...)
	argv = append(argv, s.SSH[1:]...)
	s.SSH = argv
}

// FS devuelve el sistema de archivos del extremo, conectando si es remoto.
func (s *Session) FS(ep Endpoint) (FS, error) {
	if !ep.Remote() {
		return s.local, nil
	}
	if r, ok := s.remotes[ep.Host]; ok {
		return r, nil
	}
	r, err := DialSSH(s.SSH, ep.Host, s.RemoteRoot, s.Cfg.RemoteBin, s.Compress)
	if err != nil {
		return nil, err
	}
	s.remotes[ep.Host] = r
	return r, nil
}

// Wire suma los bytes que pasaron por el cable en todas las conexiones.
func (s *Session) Wire() int64 {
	var n int64
	for _, r := range s.remotes {
		n += r.Wire()
	}
	return n
}

func (s *Session) Close() {
	for _, r := range s.remotes {
		r.Close()
	}
}

func waitMagic(r *bufio.Reader) error {
	want := []byte(agentMagic)
	matched := 0
	for n := 0; n < 1<<20; n++ {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch {
		case b == want[matched]:
			matched++
			if matched == len(want) {
				return nil
			}
		case b == want[0]:
			matched = 1
		default:
			matched = 0
		}
	}
	return errors.New("no apareció la señal del agente en el primer MiB de salida")
}

func badChars(s string) bool { return strings.ContainsAny(s, "'\"$`\\") }

// DialSSH lanza `ssh <host> vextra agent` y habla el protocolo por su
// stdin/stdout.
//
// DECISIÓN DE DISEÑO (V1 demo): se usa el binario `ssh` del sistema en vez de
// golang.org/x/crypto/ssh. A cambio de requerir OpenSSH en el CLIENTE (el remoto
// sólo necesita sshd y el binario vextra), se heredan gratis ~/.ssh/config,
// ProxyJump, ssh-agent, known_hosts, ControlMaster, FIDO, etc. Un transporte
// nativo con x/crypto/ssh encaja detrás de la misma interfaz (io.Reader/Writer).
func DialSSH(sshArgv []string, host, root, bin string, compress bool) (*RemoteFS, error) {
	if badChars(root) || badChars(bin) {
		return nil, fmt.Errorf("%w: caracteres no permitidos en --remote-root / remote_bin", ErrUsage)
	}
	rootArg := ""
	if root != "" {
		rootArg = ` --root "` + root + `"`
	}
	remoteCmd := fmt.Sprintf(`sh -c 'PATH="$HOME/.local/bin:$PATH"; exec %s agent%s'`, bin, rootArg)

	argv := append(append([]string{}, sshArgv[1:]...), host, remoteCmd)
	cmd := exec.Command(sshArgv[0], argv...)
	cmd.Stderr = os.Stderr // prompts, banners y errores de ssh/agente visibles
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: no se pudo ejecutar %q: %v", ErrConn, sshArgv[0], err)
	}
	br := bufio.NewReaderSize(stdout, 256<<10)
	if err := waitMagic(br); err != nil {
		stdin.Close()
		werr := cmd.Wait()
		return nil, connError(werr, host, err)
	}
	var conn *Conn
	closeFn := func() error {
		stdin.Close()
		timer := time.AfterFunc(5*time.Second, func() { cmd.Process.Kill() })
		defer timer.Stop()
		<-conn.done
		return cmd.Wait()
	}
	conn = NewConn(br, stdin, closeFn)
	rfs, err := NewRemoteFS(conn, compress)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return rfs, nil
}

func connError(waitErr error, host string, cause error) error {
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) && ee.ExitCode() == 127 {
		return fmt.Errorf("%w: vextra no está instalado en %s (probá: vx install-remote %s)", ErrConn, host, host)
	}
	if errors.As(waitErr, &ee) {
		return fmt.Errorf("%w: ssh a %s terminó con código %d (%v)", ErrConn, host, ee.ExitCode(), cause)
	}
	return fmt.Errorf("%w: %s: %v", ErrConn, host, cause)
}

// installRemote copia el binario actual a ~/.local/bin/vextra del remoto y
// comprueba que arranca. Requiere que ambos equipos tengan la misma arquitectura;
// si no, usar el binario de `make dist` correspondiente.
func installRemote(sshArgv []string, host string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.Open(self)
	if err != nil {
		return err
	}
	defer f.Close()
	script := `sh -c 'mkdir -p "$HOME/.local/bin" && cat > "$HOME/.local/bin/vextra.new" && chmod 755 "$HOME/.local/bin/vextra.new" && mv "$HOME/.local/bin/vextra.new" "$HOME/.local/bin/vextra"'`
	cmd := exec.Command(sshArgv[0], append(append([]string{}, sshArgv[1:]...), host, script)...)
	cmd.Stdin = f
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: falló la copia a %s: %v", ErrConn, host, err)
	}
	check := `sh -c '"$HOME/.local/bin/vextra" version'`
	out, err := exec.Command(sshArgv[0], append(append([]string{}, sshArgv[1:]...), host, check)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("el binario se copió pero no arranca en %s (¿otra arquitectura? usá `make dist`): %v\n%s", host, err, out)
	}
	fmt.Printf("instalado en %s:~/.local/bin/vextra — %s", host, out)
	return nil
}

// ParseSSHCommand importa un comando ssh ya escrito, p. ej.
//
//	ssh -p 2222 -i ~/.ssh/k -J bastion usuario@host
//
// y devuelve el host y las opciones que hay que pasarle a ssh. Descarta las
// opciones que romperían el agente (-t, -T, -N, -f, -n) y rechaza comandos con
// un comando remoto al final.
func ParseSSHCommand(cmdline string) (host string, opts []string, err error) {
	toks, err := splitArgs(cmdline)
	if err != nil {
		return "", nil, err
	}
	if len(toks) == 0 || path.Base(toks[0]) != "ssh" {
		return "", nil, errors.New("se esperaba un comando que empiece con ssh")
	}
	const withArg = "bcDEeFIiJLlmOopQRSWw" // opciones de ssh que llevan un valor aparte
	drop := map[string]bool{"-t": true, "-tt": true, "-T": true, "-N": true, "-f": true, "-n": true}
	i := 1
	for ; i < len(toks); i++ {
		t := toks[i]
		if !strings.HasPrefix(t, "-") || t == "-" {
			break
		}
		if drop[t] {
			continue
		}
		opts = append(opts, t)
		if len(t) == 2 && strings.IndexByte(withArg, t[1]) >= 0 && i+1 < len(toks) {
			i++
			opts = append(opts, toks[i])
		}
	}
	if i >= len(toks) {
		return "", nil, errors.New("no encontré el host en el comando ssh")
	}
	host = toks[i]
	if i+1 < len(toks) {
		return "", nil, errors.New("el comando ssh trae un comando remoto al final; pegá sólo ssh [opciones] host")
	}
	return host, opts, nil
}