package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
)

// HashSize es el tamaño en bytes de todos los digests del sistema.
const HashSize = 32

// hashName identifica el algoritmo en uso (se muestra en `vx config show`).
//
// PUNTO DE INTERCAMBIO: la spec pide BLAKE3. Esta build usa SHA-256 (stdlib)
// para no depender de módulos externos. Para migrar basta cambiar newHash()
// y hashName por una implementación de hash.Hash de 32 bytes (p. ej.
// lukechampine.com/blake3 o github.com/zeebo/blake3). El resto del código
// sólo usa newHash().
const hashName = "sha256"

// Digest es un hash de HashSize bytes.
type Digest [HashSize]byte

func newHash() hash.Hash { return sha256.New() }

func sumBytes(b []byte) Digest {
	h := newHash()
	h.Write(b)
	var d Digest
	copy(d[:], h.Sum(nil))
	return d
}

func (d Digest) String() string { return hex.EncodeToString(d[:]) }

func parseDigest(s string) (Digest, error) {
	var d Digest
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != HashSize {
		return d, fmt.Errorf("digest inválido: %q", s)
	}
	copy(d[:], b)
	return d, nil
}
