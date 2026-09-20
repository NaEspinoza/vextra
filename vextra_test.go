package main

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
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
