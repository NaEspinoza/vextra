package main

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var baseTime = time.Unix(1700000000, 0)

func randBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func writeFileAt(t *testing.T, p string, data []byte, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func testOpts() *Options {
	return &Options{
		MaxDelete: 1000, ConfirmMass: true, ConfirmOver: 10, Workers: 4,
		MaxInflight: 16 << 20, BlockSize: 64 << 10, SmallFile: 4 << 10,
		Resume: true, Quiet: true, Out: io.Discard, Err: io.Discard,
	}
}

// newPipeRemote levanta un agente en proceso conectado por io.Pipe: ejercita
// el protocolo real (frames, multiplexado, compresión) sin necesitar ssh.
func newPipeRemote(t *testing.T) *RemoteFS {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	go func() {
		Serve(c2sR, s2cW, NewLocalFS("", ""))
		s2cW.Close()
	}()
	conn := NewConn(s2cR, c2sW, func() error { return c2sW.Close() })
	rfs, err := NewRemoteFS(conn, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rfs.Close() })
	return rfs
}

func assertSameTree(t *testing.T, a, b string) {
	t.Helper()
	l := NewLocalFS("", "")
	ea, err := l.List(a, true)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := l.List(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ea) != len(eb) {
		t.Fatalf("distinto número de entradas: %d vs %d", len(ea), len(eb))
	}
	for i := range ea {
		x, y := ea[i], eb[i]
		if x.Path != y.Path || x.Type != y.Type || x.Link != y.Link || x.Size != y.Size {
			t.Fatalf("difieren: %+v vs %+v", x, y)
		}
		if x.Type == TFile {
			ca, _ := os.ReadFile(filepath.Join(a, x.Path))
			cb, _ := os.ReadFile(filepath.Join(b, y.Path))
			if !bytes.Equal(ca, cb) {
				t.Fatalf("contenido distinto en %s", x.Path)
			}
		}
	}
}

func TestSyncLocalTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFileAt(t, filepath.Join(src, "empty"), nil, baseTime)
	writeFileAt(t, filepath.Join(src, "small.txt"), []byte("hola"), baseTime)
	writeFileAt(t, filepath.Join(src, "a/b/big.bin"), randBytes(300<<10, 1), baseTime)
	if err := os.Symlink("small.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	l := NewLocalFS("", "")
	o := testOpts()
	if _, err := Run(l, src, l, dst, o, false, nil); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, src, dst)
	res, err := Run(l, src, l, dst, o, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.HasChanges() {
		t.Fatalf("la segunda corrida debería estar al día: %s", res.Plan.Summary())
	}
}

func TestDeltaOverRemote(t *testing.T) {
	src, dstRoot := t.TempDir(), t.TempDir()
	big := randBytes(4<<20, 7) // 64 bloques de 64 KiB, incompresibles
	sp := filepath.Join(src, "big.bin")
	writeFileAt(t, sp, big, baseTime)
	l := NewLocalFS("", "")
	r := newPipeRemote(t)
	o := testOpts()
	dst := filepath.Join(dstRoot, "proj")

	res, err := Run(l, src, r, dst, o, false, r.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Stats.SentBytes.Load(); got != 4<<20 {
		t.Fatalf("primera copia: se esperaban %d bytes de contenido, hubo %d", 4<<20, got)
	}
	assertSameTree(t, src, dst)

	// Editar un solo bloque.
	big[1<<20+100] ^= 0xFF
	writeFileAt(t, sp, big, baseTime.Add(10*time.Second))
	w0 := r.Wire()
	res, err = Run(l, src, r, dst, o, false, r.Wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Stats.SentBytes.Load(); got != 64<<10 {
		t.Fatalf("delta: se esperaba 1 bloque (%d bytes), hubo %d", 64<<10, got)
	}
	if wire := r.Wire() - w0; wire > 300<<10 {
		t.Fatalf("delta: demasiado tráfico en el cable: %d bytes", wire)
	}
	assertSameTree(t, src, dst)
}

func TestPullFromRemote(t *testing.T) {
	remoteDir, local := t.TempDir(), filepath.Join(t.TempDir(), "copia")
	writeFileAt(t, filepath.Join(remoteDir, "x/data.bin"), randBytes(200<<10, 2), baseTime)
	writeFileAt(t, filepath.Join(remoteDir, "notes.txt"), bytes.Repeat([]byte("linea de texto\n"), 2000), baseTime)
	r := newPipeRemote(t)
	if _, err := Run(r, remoteDir, NewLocalFS("", ""), local, testOpts(), false, nil); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, remoteDir, local)
}

func TestResumeReusesTemp(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(1<<20, 3) // 16 bloques de 64 KiB
	sp, dp := filepath.Join(src, "f.bin"), filepath.Join(dst, "f.bin")
	writeFileAt(t, sp, data, baseTime)
	l := NewLocalFS("", "")
	o := testOpts()

	// Simula un corte: la mitad de los bloques quedó en el temporal del job.
	job := jobID(dp, int64(len(data)), baseTime.UnixNano(), true)
	w, err := l.Begin(dp, job, int64(len(data)), false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		lo, hi := i*o.BlockSize, (i+1)*o.BlockSize
		if err := w.WriteAt(data[lo:hi], int64(lo)); err != nil {
			t.Fatal(err)
		}
	}
	w.Abort()
	if _, err := os.Lstat(dp); err == nil {
		t.Fatal("el destino final no debería existir todavía")
	}

	res, err := Run(l, sp, l, dp, o, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Stats.SentBytes.Load(); got != int64(len(data))/2 {
		t.Fatalf("reanudación: se esperaba enviar la mitad (%d), se envió %d", len(data)/2, got)
	}
	assertSameTree(t, src, dst)
	left, _ := filepath.Glob(filepath.Join(dst, "*"+tmpMarker+"*"))
	if len(left) != 0 {
		t.Fatalf("quedaron temporales: %v", left)
	}
}

func TestMtimeOnlyChange(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(200<<10, 4)
	sp := filepath.Join(src, "f.bin")
	writeFileAt(t, sp, data, baseTime)
	l := NewLocalFS("", "")
	o := testOpts()
	if _, err := Run(l, src, l, dst, o, false, nil); err != nil {
		t.Fatal(err)
	}
	newer := baseTime.Add(time.Hour)
	if err := os.Chtimes(sp, newer, newer); err != nil {
		t.Fatal(err)
	}
	res, err := Run(l, src, l, dst, o, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.MetaOnly.Load() != 1 || res.Stats.SentBytes.Load() != 0 {
		t.Fatalf("se esperaba 1 archivo sólo-metadatos y 0 bytes enviados, hubo %d y %d",
			res.Stats.MetaOnly.Load(), res.Stats.SentBytes.Load())
	}
	fi, err := os.Stat(filepath.Join(dst, "f.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(newer) {
		t.Fatalf("mtime no propagado: %v", fi.ModTime())
	}
}

func TestTypeChangeReplace(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFileAt(t, filepath.Join(src, "x/inner.txt"), []byte("adentro"), baseTime)
	writeFileAt(t, filepath.Join(dst, "x"), []byte("yo era un archivo"), baseTime)
	l := NewLocalFS("", "")
	if _, err := Run(l, src, l, dst, testOpts(), false, nil); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, src, dst)
}

func TestVerifyMismatchLocalAndRemote(t *testing.T) {
	for _, remote := range []bool{false, true} {
		var f FS = NewLocalFS("", "")
		if remote {
			f = newPipeRemote(t)
		}
		dp := filepath.Join(t.TempDir(), "x")
		w, err := f.Begin(dp, "job1", 5, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WriteAt([]byte("hello"), 0); err != nil {
			t.Fatal(err)
		}
		err = w.Commit(sumBytes([]byte("otro")), 0644, baseTime.UnixNano())
		if !errors.Is(err, ErrVerify) {
			t.Fatalf("remote=%v: se esperaba ErrVerify, hubo %v", remote, err)
		}
		if _, err := os.Lstat(dp); err == nil {
			t.Fatalf("remote=%v: el destino no debe existir tras un fallo de verificación", remote)
		}
		if _, err := os.Lstat(dp + tmpMarker + "job1"); err == nil {
			t.Fatalf("remote=%v: el temporal corrupto debe borrarse", remote)
		}
	}
}

func TestDeleteSafeguards(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFileAt(t, filepath.Join(src, "keep"), []byte("k"), baseTime)
	for i := 0; i < 5; i++ {
		writeFileAt(t, filepath.Join(dst, "extra"+string(rune('a'+i))), []byte("x"), baseTime)
	}
	l := NewLocalFS("", "")
	o := testOpts()
	o.Delete = true

	o.MaxDelete = 3
	if _, err := Run(l, src, l, dst, o, false, nil); !errors.Is(err, ErrSafety) {
		t.Fatalf("--max-delete: se esperaba ErrSafety, hubo %v", err)
	}
	if ents, _ := l.List(dst, false); len(ents) != 5 {
		t.Fatalf("un aborto por salvaguarda no debe modificar nada (hay %d entradas)", len(ents))
	}

	o.MaxDelete, o.ConfirmOver = 10, 3
	if _, err := Run(l, src, l, dst, o, false, nil); !errors.Is(err, ErrSafety) {
		t.Fatalf("sin confirmación: se esperaba ErrSafety, hubo %v", err)
	}
	if ents, _ := l.List(dst, false); len(ents) != 5 {
		t.Fatalf("sin confirmar no debe modificar nada (hay %d entradas)", len(ents))
	}

	o.Confirm = func(string) bool { return true }
	if _, err := Run(l, src, l, dst, o, false, nil); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, src, dst)
}

func TestDryRunChangesNothing(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "nuevo")
	writeFileAt(t, filepath.Join(src, "a.txt"), []byte("a"), baseTime)
	l := NewLocalFS("", "")
	var out bytes.Buffer
	o := testOpts()
	o.Out = &out
	res, err := Run(l, src, l, dst, o, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Plan.HasChanges() || out.Len() == 0 {
		t.Fatal("el dry-run debería listar cambios")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("el dry-run creó el destino")
	}
}

func TestPathSafety(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../x", "a/../b", "/abs", "a//b", "a/./b"} {
		if validRel(bad) {
			t.Errorf("validRel(%q) debería ser false", bad)
		}
	}
	for _, good := range []string{"a", "a/b", "dir/archivo.txt", "..oculto"} {
		if !validRel(good) {
			t.Errorf("validRel(%q) debería ser true", good)
		}
	}
	if err := checkEntries([]Entry{{Path: "evil", Type: TLink, Link: "/etc"}, {Path: "evil/passwd", Type: TFile}}); err == nil {
		t.Error("checkEntries debería rechazar una entrada dentro de un symlink")
	}
	if err := checkEntries([]Entry{{Path: "../fuera", Type: TFile}}); err == nil {
		t.Error("checkEntries debería rechazar traversal")
	}

	root, other := t.TempDir(), t.TempDir()
	l := NewLocalFS(root, root)
	if _, err := l.Stat("/etc/passwd", false); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("ruta absoluta fuera de la raíz: %v", err)
	}
	if _, err := l.Stat(root+"/../etc", false); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("traversal con ..: %v", err)
	}
	if err := os.Symlink(other, filepath.Join(root, "esc")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Stat(root+"/esc/x", false); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("symlink que escapa de la raíz: %v", err)
	}
	if e, err := l.Stat(root+"/esc", false); err != nil || e.Type != TLink {
		t.Errorf("el symlink en sí debe poder consultarse: %v %+v", err, e)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	raw := bytes.Repeat([]byte("vextra "), 2000)
	cb, ok := compressBlock(raw)
	if !ok {
		t.Fatal("un texto repetitivo debería comprimirse")
	}
	var buf bytes.Buffer
	m := Msg{Op: "write", H: 3, Off: 42, RawLen: len(raw)}
	if _, err := writeFrame(&buf, 9, flagZ, &m, cb); err != nil {
		t.Fatal(err)
	}
	f, err := readFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if f.id != 9 || f.msg.Op != "write" || f.msg.Off != 42 || !bytes.Equal(f.payload, raw) {
		t.Fatalf("frame mal decodificado: %+v", f.msg)
	}
	if _, ok := compressBlock(randBytes(8192, 1)); ok {
		t.Fatal("datos aleatorios no deberían pasar por compresión")
	}
}

func TestConfigParse(t *testing.T) {
	c := DefaultConfig()
	err := c.parse("compression: auto\nresume: false\nsafety:\n  max_delete: 5\n  confirm_mass_delete: false\nworkers: 3\nmax_inflight: 64MiB # comentario\nblock_size: 256KiB\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Resume || c.MaxDelete != 5 || c.ConfirmMass || c.Workers != 3 || c.MaxInflight != 64<<20 || c.BlockSize != 256<<10 {
		t.Fatalf("config mal parseada: %+v", c)
	}
	if err := c.parse("hash: blake3\n"); err == nil {
		t.Fatal("hash blake3 debería rechazarse en esta build")
	}
	if err := c.parse("inexistente: 1\n"); err == nil {
		t.Fatal("claves desconocidas deben rechazarse")
	}
}

func TestParsersAndHelpers(t *testing.T) {
	eps := map[string]Endpoint{
		"host:/srv/x":    {Host: "host", Path: "/srv/x"},
		"me@host:dir":    {Host: "me@host", Path: "dir"},
		"host:":          {Host: "host", Path: "."},
		"./a:b":          {Path: "./a:b"},
		"/tmp/a:b":       {Path: "/tmp/a:b"},
		"relativo/dir":   {Path: "relativo/dir"},
		"solo-un-nombre": {Path: "solo-un-nombre"},
	}
	for in, want := range eps {
		if got := ParseEndpoint(in); got != want {
			t.Errorf("ParseEndpoint(%q) = %+v, se esperaba %+v", in, got, want)
		}
	}
	sizes := map[string]int64{"1024": 1024, "64KiB": 64 << 10, "256MiB": 256 << 20, "1G": 1 << 30, "20MB": 20 << 20}
	for in, want := range sizes {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
	args, err := splitArgs(`put "mi archivo.txt" 'otro dir'/x  a\ b`)
	if err != nil || len(args) != 4 || args[1] != "mi archivo.txt" || args[2] != "otro dir/x" || args[3] != "a b" {
		t.Errorf("splitArgs: %q %v", args, err)
	}
}

func TestParseSSHCommand(t *testing.T) {
	host, opts, err := ParseSSHCommand(`ssh -p 2222 -i ~/.ssh/k -o StrictHostKeyChecking=no -t -J bastion me@srv`)
	if err != nil || host != "me@srv" {
		t.Fatalf("host=%q err=%v", host, err)
	}
	if got := strings.Join(opts, " "); got != "-p 2222 -i ~/.ssh/k -o StrictHostKeyChecking=no -J bastion" {
		t.Fatalf("opciones: %q", got)
	}
	for _, bad := range []string{"ssh me@srv ls -la", "scp a b", "ssh -p", "", "ssh"} {
		if _, _, err := ParseSSHCommand(bad); err == nil {
			t.Errorf("ParseSSHCommand(%q) debería fallar", bad)
		}
	}
}

func TestSSHFlagsToArgs(t *testing.T) {
	fl := newFlagSet("t")
	x := &xferFlags{}
	x.registerSSH(fl)
	pos, err := parseFlags(fl, []string{"ssh", "-p", "2222", "--identity", "/k", "-o", "A=1", "-o", "B=2", "me@h"}, x)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(x.sshArgs(), " "); got != "-p 2222 -i /k -o A=1 -o B=2" {
		t.Fatalf("sshArgs: %q", got)
	}
	host, extra, err := splitSSHTarget(pos)
	if err != nil || host != "me@h" || len(extra) != 0 {
		t.Fatalf("splitSSHTarget: %q %v %v", host, extra, err)
	}
	host, extra, err = splitSSHTarget([]string{"ssh -p 22 -i k me@h"})
	if err != nil || host != "me@h" || strings.Join(extra, " ") != "-p 22 -i k" {
		t.Fatalf("comando pegado: %q %v %v", host, extra, err)
	}
}

func TestSSHOptionPrecedence(t *testing.T) {
	t.Setenv("VX_SSH", "") // el entorno del usuario no debe influir en el resultado
	cfg := DefaultConfig()

	// Lo explícito va antes que el comando base: ssh se queda con el primer valor.
	s := NewSession(&cfg, "ssh -p 22 -i default", "", false)
	s.AddSSHOptions("-p", "2222")
	if got := strings.Join(s.SSH, " "); got != "ssh -p 2222 -p 22 -i default" {
		t.Fatalf("AddSSHOptions: %q", got)
	}
	s.AddSSHOptions()
	if got := strings.Join(s.SSH, " "); got != "ssh -p 2222 -p 22 -i default" {
		t.Fatalf("sin opciones no debe cambiar nada: %q", got)
	}

	// Flags primero, después lo importado de un comando pegado, al final el base.
	x := &xferFlags{port: "2222"}
	s = newSession(&cfg, x, "", false, []string{"-i", "k"})
	if got := strings.Join(s.SSH, " "); got != "ssh -p 2222 -i k" {
		t.Fatalf("newSession: %q", got)
	}
	x = &xferFlags{ssh: "ssh -p 22", port: "2222"}
	s = newSession(&cfg, x, "", false, []string{"-i", "k"})
	if got := strings.Join(s.SSH, " "); got != "ssh -p 2222 -i k -p 22" {
		t.Fatalf("newSession con --ssh: %q", got)
	}
}

func TestCLIExitCodes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // aísla el test de una config real del usuario
	t.Setenv("VX_CONFIG", "")
	src, dst := t.TempDir(), t.TempDir()
	writeFileAt(t, filepath.Join(src, "a.txt"), []byte("hola"), baseTime)
	cli := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := run(args, strings.NewReader(""), &out, &errb)
		return code, out.String() + errb.String()
	}

	if code, _ := cli("nope"); code != exitUsage {
		t.Errorf("comando desconocido: código %d, se esperaba %d", code, exitUsage)
	}
	if code, out := cli("sync", "-h"); code != exitOK || !strings.Contains(out, "Uso:") {
		t.Errorf("sync -h: código %d, salida %q", code, out)
	}
	if code, _ := cli("put", "host:/x", dst); code != exitUsage {
		t.Errorf("put con origen remoto: código %d, se esperaba %d", code, exitUsage)
	}
	// Los flags pueden ir después de los posicionales.
	if code, _ := cli("diff", src, dst, "--exit-code", "-q"); code != exitDiffs {
		t.Errorf("diff con diferencias: código %d, se esperaba %d", code, exitDiffs)
	}
	if code, out := cli("sync", src, dst, "-q"); code != exitOK {
		t.Fatalf("sync: código %d: %s", code, out)
	}
	if code, _ := cli("diff", src, dst, "--exit-code", "-q"); code != exitOK {
		t.Errorf("diff sin diferencias: código %d, se esperaba %d", code, exitOK)
	}
	// --max-delete 0 + --delete: cualquier borrado aborta con código de salvaguarda.
	writeFileAt(t, filepath.Join(dst, "sobrante.txt"), []byte("x"), baseTime)
	if code, _ := cli("sync", src, dst, "--delete", "--max-delete", "0", "-q"); code != exitSafety {
		t.Errorf("--max-delete 0: código %d, se esperaba %d", code, exitSafety)
	}
	if _, err := os.Stat(filepath.Join(dst, "sobrante.txt")); err != nil {
		t.Errorf("un aborto por salvaguarda no debe borrar nada: %v", err)
	}
}

func TestExcludeMatching(t *testing.T) {
	es, err := compileExcludes([]string{"*.log", "node_modules/", "cache/tmp", "", "# comentario"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		rel  string
		typ  string
		want bool
	}{
		{"a.log", TFile, true},                  // sin "/": por nombre, en la raíz
		{"src/deep/b.log", TFile, true},          // sin "/": por nombre, a cualquier profundidad
		{"node_modules", TDir, true},             // el directorio en sí
		{"node_modules/pkg/index.js", TFile, true}, // heredado de un ancestro excluido
		{"cache/tmp", TFile, true},                // anclado, coincide exacto
		{"cache/tmpX", TFile, false},              // anclado: no es un prefijo, path.Match no matchea
		{"cache/otro", TFile, false},
		{"notes.txt", TFile, false},
	}
	for _, c := range cases {
		if got := es.Excluded(c.rel, c.typ); got != c.want {
			t.Errorf("Excluded(%q, %q) = %v, se esperaba %v", c.rel, c.typ, got, c.want)
		}
	}
	if _, err := compileExcludes([]string{"[abc"}); err == nil {
		t.Error("un patrón inválido debería fallar al compilar")
	}
	if _, err := compileExcludes([]string{"/"}); err == nil {
		t.Error("un patrón vacío tras quitar '/' debería fallar al compilar")
	}
}

func TestSyncWithExcludes(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFileAt(t, filepath.Join(src, "keep.txt"), []byte("k"), baseTime)
	writeFileAt(t, filepath.Join(src, "debug.log"), []byte("d"), baseTime)
	writeFileAt(t, filepath.Join(src, "node_modules/pkg/index.js"), []byte("x"), baseTime)
	// Lo excluido que ya está en destino no se borra ni con --delete.
	writeFileAt(t, filepath.Join(dst, "debug.log"), []byte("viejo"), baseTime)

	es, err := compileExcludes([]string{"*.log", "node_modules/"})
	if err != nil {
		t.Fatal(err)
	}
	l := NewLocalFS("", "")
	o := testOpts()
	o.Exclude = es
	o.Delete = true

	plan, err := BuildPlan(l, src, l, dst, o)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Excluded == 0 {
		t.Error("Plan.Excluded debería contar las entradas de origen ignoradas")
	}
	for _, a := range plan.Actions {
		if strings.Contains(a.Rel, ".log") || strings.Contains(a.Rel, "node_modules") {
			t.Errorf("acción inesperada sobre una ruta excluida: %+v", a)
		}
	}
	for _, e := range plan.Extra {
		if strings.Contains(e.Path, ".log") {
			t.Errorf("una ruta excluida no debería listarse para borrar: %+v", e)
		}
	}

	if _, err := Run(l, src, l, dst, o, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Errorf("keep.txt debería haberse copiado: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules")); err == nil {
		t.Error("node_modules no debería haberse copiado")
	}
	got, err := os.ReadFile(filepath.Join(dst, "debug.log"))
	if err != nil || string(got) != "viejo" {
		t.Errorf("debug.log excluido no debería tocarse: %q, %v", got, err)
	}
}

func TestDirectoryMTimePreserved(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	dirTime := baseTime.Add(-48 * time.Hour)
	writeFileAt(t, filepath.Join(src, "sub/a.txt"), []byte("a"), baseTime)
	if err := os.Chtimes(filepath.Join(src, "sub"), dirTime, dirTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, dirTime, dirTime); err != nil {
		t.Fatal(err)
	}
	l := NewLocalFS("", "")
	if _, err := Run(l, src, l, dst, testOpts(), false, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dst, filepath.Join(dst, "sub")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(dirTime) {
			t.Errorf("%s: mtime = %v, se esperaba %v (crear el archivo adentro no debería pisarlo)", p, fi.ModTime(), dirTime)
		}
	}
}

func TestAdaptiveBlockSize(t *testing.T) {
	cases := []struct{ size int64 }{{100 << 10}, {50 << 20}, {2 << 30}}
	for _, c := range cases {
		bs := blockSizeFor(c.size, 0)
		if bs < minBlockSize || bs > maxBlockSize {
			t.Errorf("blockSizeFor(%d) = %d, fuera de [%d, %d]", c.size, bs, minBlockSize, maxBlockSize)
		}
		if bs&(bs-1) != 0 {
			t.Errorf("blockSizeFor(%d) = %d, no es potencia de dos", c.size, bs)
		}
	}
	if bs := blockSizeFor(999<<20, 128<<10); bs != 128<<10 {
		t.Errorf("un tamaño fijo debe respetarse: %d", bs)
	}
	// Con un tamaño de bloque más chico para un archivo grande, editar un 1%
	// debería tocar muchos menos bloques que con el 1MiB fijo de antes.
	if bs := blockSizeFor(64<<20, 0); bs >= 1<<20 {
		t.Errorf("un archivo de 64MiB debería usar un bloque adaptativo menor a 1MiB, dio %d", bs)
	}
}

func TestDeepDiffMatchesRealTransfer(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	big := randBytes(20<<20, 11)
	sp := filepath.Join(src, "big.bin")
	writeFileAt(t, sp, big, baseTime)
	l := NewLocalFS("", "")
	o := testOpts()
	if _, err := Run(l, src, l, dst, o, false, nil); err != nil {
		t.Fatal(err)
	}
	// Editar ~1% del archivo.
	edit := len(big) / 100
	for i := 0; i < edit; i++ {
		big[1<<20+i] ^= 0xFF
	}
	writeFileAt(t, sp, big, baseTime.Add(time.Minute))

	o.Deep = true
	res, err := Run(l, src, l, dst, o, true, nil) // dry-run con --deep
	if err != nil {
		t.Fatal(err)
	}
	if !res.Plan.Deep || res.Plan.DeltaBytes == 0 {
		t.Fatalf("se esperaba un delta calculado y no nulo: %+v", res.Plan)
	}
	if res.Plan.DeltaBytes >= res.Plan.Bytes {
		t.Fatalf("editar ~1%% debería estimar mucho menos que el archivo completo: delta=%d bytes=%d",
			res.Plan.DeltaBytes, res.Plan.Bytes)
	}
	estimated := res.Plan.DeltaBytes

	o.Deep = false
	real, err := Run(l, src, l, dst, o, false, nil) // corrida real
	if err != nil {
		t.Fatal(err)
	}
	if got := real.Stats.SentBytes.Load(); got != estimated {
		t.Fatalf("--deep estimó %d bytes pero la corrida real movió %d", estimated, got)
	}
	if got := real.Stats.SentBytes.Load(); float64(got) > float64(len(big))*0.05 {
		t.Fatalf("editar ~1%% no debería mover más de un 5%% del archivo: %d de %d bytes", got, len(big))
	}
}

func TestBuildExcludes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Excludes = []string{"*.bak"}
	dir := t.TempDir()
	ff := filepath.Join(dir, "excl.txt")
	if err := os.WriteFile(ff, []byte("*.tmp\n# comentario\n\nnode_modules/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	x := &xferFlags{excludeFrom: ff, excludes: multiFlag{"*.log"}}
	ex, err := buildExcludes(&cfg, x)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a.bak", "a.tmp", "a.log"} {
		if !ex.Excluded(want, TFile) {
			t.Errorf("se esperaba que %q esté excluido (config + exclude-from + --exclude combinados)", want)
		}
	}
	if ex.Excluded("a.txt", TFile) {
		t.Error("a.txt no debería estar excluido")
	}
	if _, err := buildExcludes(&cfg, &xferFlags{excludeFrom: "/no/existe"}); err == nil {
		t.Error("--exclude-from con un archivo inexistente debería fallar")
	}
	if ex2, err := buildExcludes(&DefaultConfig(), &xferFlags{}); err != nil || ex2 != nil {
		t.Errorf("sin exclusiones, buildExcludes debería devolver (nil, nil): %v, %v", ex2, err)
	}
}