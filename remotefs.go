package main

import (
	"encoding/json"
	"fmt"
	"sync"
)

// RemoteFS implementa FS contra un agente remoto a través de una Conn.
// La compresión es un detalle de este nivel: el motor de sincronización
// no sabe (ni le importa) qué viaja comprimido.
type RemoteFS struct {
	c        *Conn
	home     string
	host     string
	arch     string
	compress bool
	comp     sync.Map // ruta -> *compState (memoria adaptativa de lecturas)
}

func NewRemoteFS(c *Conn, compress bool) (*RemoteFS, error) {
	f, err := c.Call(Msg{Op: "hello", Ver: protoVersion}, nil, false)
	if err != nil {
		return nil, err
	}
	if f.msg.Ver != protoVersion {
		return nil, fmt.Errorf("versión de protocolo incompatible: cliente=%d agente=%d", protoVersion, f.msg.Ver)
	}
	return &RemoteFS{c: c, home: f.msg.Home, host: f.msg.Host, arch: f.msg.Arch, compress: compress}, nil
}

func (r *RemoteFS) Home() string { return r.home }
func (r *RemoteFS) Close() error { return r.c.Close() }

// Wire devuelve los bytes totales (in+out) que pasaron por el cable.
func (r *RemoteFS) Wire() int64 { return r.c.BytesIn.Load() + r.c.BytesOut.Load() }

func (r *RemoteFS) Stat(p string, follow bool) (*Entry, error) {
	f, err := r.c.Call(Msg{Op: "stat", Path: p, Follow: follow}, nil, false)
	if err != nil {
		return nil, err
	}
	if f.msg.Entry == nil {
		return nil, fmt.Errorf("respuesta sin entrada para %s", p)
	}
	return f.msg.Entry, nil
}

func (r *RemoteFS) List(p string, recursive bool) ([]Entry, error) {
	f, err := r.c.Call(Msg{Op: "list", Path: p, Recursive: recursive, Z: r.compress}, nil, false)
	if err != nil {
		return nil, err
	}
	var ents []Entry
	if err := json.Unmarshal(f.payload, &ents); err != nil {
		return nil, fmt.Errorf("listado inválido: %w", err)
	}
	return ents, nil
}

func (r *RemoteFS) simple(m Msg) error {
	_, err := r.c.Call(m, nil, false)
	return err
}

func (r *RemoteFS) Mkdir(p string, mode uint32) error {
	return r.simple(Msg{Op: "mkdir", Path: p, Mode: mode})
}

func (r *RemoteFS) Remove(p string, recursive bool) error {
	return r.simple(Msg{Op: "rm", Path: p, Recursive: recursive})
}

func (r *RemoteFS) Rename(from, to string) error {
	return r.simple(Msg{Op: "mv", Path: from, To: to})
}

func (r *RemoteFS) Symlink(target, p string) error {
	return r.simple(Msg{Op: "symlink", Path: p, To: target})
}

func (r *RemoteFS) SetMeta(p string, mode uint32, mtime int64) error {
	return r.simple(Msg{Op: "setmeta", Path: p, Mode: mode, MTime: mtime})
}

func (r *RemoteFS) Sigs(p, job string, bs int) (*Sigs, error) {
	f, err := r.c.Call(Msg{Op: "sigs", Path: p, Job: job, BS: bs}, nil, false)
	if err != nil {
		return nil, err
	}
	s := &Sigs{Exists: f.msg.Exists, Kind: f.msg.Kind, Size: f.msg.Size, BS: f.msg.BS}
	if !s.Exists {
		return s, nil
	}
	if s.Hash, err = parseDigest(f.msg.Hash); err != nil {
		return nil, err
	}
	if len(f.payload)%HashSize != 0 {
		return nil, fmt.Errorf("firmas corruptas (%d bytes)", len(f.payload))
	}
	s.Blocks = make([]Digest, len(f.payload)/HashSize)
	for i := range s.Blocks {
		copy(s.Blocks[i][:], f.payload[i*HashSize:(i+1)*HashSize])
	}
	return s, nil
}

func (r *RemoteFS) stateFor(p string) *compState {
	if v, ok := r.comp.Load(p); ok {
		return v.(*compState)
	}
	v, _ := r.comp.LoadOrStore(p, newCompState(p))
	return v.(*compState)
}

func (r *RemoteFS) ReadAt(p string, off int64, n int) ([]byte, error) {
	st := r.stateFor(p)
	try := r.compress && st.try()
	f, err := r.c.Call(Msg{Op: "read", Path: p, Off: off, Len: n, Z: try}, nil, false)
	if err != nil {
		return nil, err
	}
	if try {
		st.report(f.flags&flagZ != 0)
	}
	return f.payload, nil
}

func (r *RemoteFS) Begin(p, job string, size int64, useBase bool) (Writer, error) {
	f, err := r.c.Call(Msg{Op: "begin", Path: p, Job: job, Size: size, UseBase: useBase}, nil, false)
	if err != nil {
		return nil, err
	}
	return &remoteWriter{c: r.c, h: f.msg.H, compress: r.compress, st: newCompState(p)}, nil
}

type remoteWriter struct {
	c        *Conn
	h        uint32
	compress bool
	st       *compState
}

func (w *remoteWriter) WriteAt(p []byte, off int64) error {
	m := Msg{Op: "write", H: w.h, Off: off}
	payload, z := p, false
	if w.compress && w.st.try() {
		cb, ok := compressBlock(p)
		w.st.report(ok)
		if ok {
			payload, z = cb, true
			m.RawLen = len(p)
		}
	}
	_, err := w.c.Call(m, payload, z)
	return err
}

func (w *remoteWriter) Commit(sum Digest, mode uint32, mtime int64) error {
	_, err := w.c.Call(Msg{Op: "commit", H: w.h, Hash: sum.String(), Mode: mode, MTime: mtime}, nil, false)
	return err
}

func (w *remoteWriter) Abort() {
	w.c.Call(Msg{Op: "abort", H: w.h}, nil, false)
}
