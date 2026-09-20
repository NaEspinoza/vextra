package main

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// codecName identifica el códec de compresión en uso.
//
// PUNTO DE INTERCAMBIO: la spec pide zstd. Esta build usa DEFLATE (stdlib,
// nivel "best speed"). Para migrar, reemplazar compressBlock/decompressBlock
// (github.com/klauspost/compress/zstd con SpeedFastest). El contrato es:
// compressBlock devuelve (datos, true) sólo si vale la pena comprimir.
const codecName = "flate"

var flatePool = sync.Pool{New: func() any {
	w, _ := flate.NewWriter(io.Discard, flate.BestSpeed)
	return w
}}

// compressBlock comprime src y devuelve (out, true) únicamente si el ahorro
// es de al menos ~10%; si no, (nil, false) y el llamador envía los datos crudos.
func compressBlock(src []byte) ([]byte, bool) {
	if len(src) < 512 {
		return nil, false
	}
	var buf bytes.Buffer
	buf.Grow(len(src) / 2)
	w := flatePool.Get().(*flate.Writer)
	w.Reset(&buf)
	_, err := w.Write(src)
	if err == nil {
		err = w.Close()
	}
	flatePool.Put(w)
	if err != nil {
		return nil, false
	}
	if buf.Len() >= len(src)-len(src)/10 {
		return nil, false
	}
	return buf.Bytes(), true
}

// decompressBlock descomprime src, que debe producir exactamente rawLen bytes.
func decompressBlock(src []byte, rawLen int) ([]byte, error) {
	if rawLen < 0 || rawLen > maxFrame {
		return nil, fmt.Errorf("largo descomprimido inválido: %d", rawLen)
	}
	r := flate.NewReader(bytes.NewReader(src))
	defer r.Close()
	out := make([]byte, rawLen)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("descompresión: %w", err)
	}
	return out, nil
}

var incompressibleExt = map[string]bool{
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true,
	".7z": true, ".rar": true, ".jar": true, ".apk": true, ".deb": true, ".rpm": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true,
	".mp3": true, ".mp4": true, ".mkv": true, ".avi": true, ".mov": true, ".webm": true,
	".ogg": true, ".flac": true, ".aac": true, ".m4a": true,
}

// worthCompressing descarta de entrada los formatos que ya vienen comprimidos.
func worthCompressing(p string) bool {
	return !incompressibleExt[strings.ToLower(filepath.Ext(p))]
}

// compState da memoria por archivo a la decisión adaptativa: tras 2 intentos
// seguidos sin ahorro real se deja de gastar CPU comprimiendo ese archivo.
type compState struct{ misses atomic.Int32 }

func newCompState(p string) *compState {
	st := &compState{}
	if !worthCompressing(p) {
		st.misses.Store(2)
	}
	return st
}

func (c *compState) try() bool { return c.misses.Load() < 2 }

func (c *compState) report(ok bool) {
	if ok {
		c.misses.Store(0)
	} else {
		c.misses.Add(1)
	}
}
