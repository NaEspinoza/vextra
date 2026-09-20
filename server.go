package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
)

// Server es el agente remoto: traduce mensajes del protocolo a llamadas
// sobre un LocalFS. Atiende requests concurrentes (hasta 64 a la vez).
type Server struct {
	fs      *LocalFS
	w       io.Writer
	wmu     sync.Mutex
	hmu     sync.Mutex
	handles map[uint32]Writer
	next    uint32
	sem     chan struct{}
	host    string
}

// Serve atiende el protocolo sobre r/w hasta que r llegue a EOF.
// NUNCA debe escribir nada en w fuera de los frames (stdout es el protocolo).
func Serve(r io.Reader, w io.Writer, l *LocalFS) error {
	host, _ := os.Hostname()
	s := &Server{fs: l, w: w, handles: map[uint32]Writer{}, sem: make(chan struct{}, 64), host: host}
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		s.hmu.Lock()
		for _, h := range s.handles {
			h.Abort() // se conservan los temporales: permiten reanudar
		}
		s.hmu.Unlock()
	}()
	for {
		f, err := readFrame(r)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		s.sem <- struct{}{} // contrapresión: si hay 64 en curso, se deja de leer
		wg.Add(1)
		go func(f *frame) {
			defer wg.Done()
			defer func() { <-s.sem }()
			resp, payload, allowZ := s.handle(f)
			s.reply(f.id, f.msg.Z, resp, payload, allowZ)
		}(f)
	}
}

func (s *Server) reply(id uint32, clientZ bool, m Msg, payload []byte, allowZ bool) {
	flags := byte(flagResp)
	if allowZ && clientZ {
		if cb, ok := compressBlock(payload); ok {
			m.RawLen = len(payload)
			payload = cb
			flags |= flagZ
		}
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	writeFrame(s.w, id, flags, &m, payload)
}

func errMsg(err error) Msg {
	return Msg{Err: err.Error(), Code: codeFor(err)}
}

func (s *Server) takeHandle(h uint32) (Writer, error) {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	w, ok := s.handles[h]
	if !ok {
		return nil, fmt.Errorf("handle inválido: %d", h)
	}
	return w, nil
}

func (s *Server) dropHandle(h uint32) {
	s.hmu.Lock()
	delete(s.handles, h)
	s.hmu.Unlock()
}

func (s *Server) handle(f *frame) (resp Msg, payload []byte, allowZ bool) {
	m := &f.msg
	defer func() {
		if r := recover(); r != nil {
			resp = errMsg(fmt.Errorf("pánico en el agente: %v", r))
			payload = nil
			allowZ = false
		}
	}()
	switch m.Op {
	case "hello":
		if m.Ver != protoVersion {
			return errMsg(fmt.Errorf("versión de protocolo incompatible: cliente=%d agente=%d", m.Ver, protoVersion)), nil, false
		}
		return Msg{Ver: protoVersion, Home: s.fs.Home(), Host: s.host, Arch: runtime.GOOS + "/" + runtime.GOARCH}, nil, false

	case "stat":
		e, err := s.fs.Stat(m.Path, m.Follow)
		if err != nil {
			return errMsg(err), nil, false
		}
		return Msg{Entry: e}, nil, false

	case "list":
		ents, err := s.fs.List(m.Path, m.Recursive)
		if err != nil {
			return errMsg(err), nil, false
		}
		b, err := json.Marshal(ents)
		if err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, b, true

	case "mkdir":
		if err := s.fs.Mkdir(m.Path, m.Mode); err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, nil, false

	case "rm":
		if err := s.fs.Remove(m.Path, m.Recursive); err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, nil, false

	case "mv":
		if err := s.fs.Rename(m.Path, m.To); err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, nil, false

	case "symlink":
		if err := s.fs.Symlink(m.To, m.Path); err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, nil, false

	case "setmeta":
		if err := s.fs.SetMeta(m.Path, m.Mode, m.MTime); err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, nil, false

	case "sigs":
		sg, err := s.fs.Sigs(m.Path, m.Job, m.BS)
		if err != nil {
			return errMsg(err), nil, false
		}
		buf := make([]byte, 0, len(sg.Blocks)*HashSize)
		for i := range sg.Blocks {
			buf = append(buf, sg.Blocks[i][:]...)
		}
		return Msg{Exists: sg.Exists, Kind: sg.Kind, Size: sg.Size, BS: sg.BS, Hash: hex.EncodeToString(sg.Hash[:])}, buf, false

	case "read":
		if m.Len < 0 || m.Len > 64<<20 {
			return errMsg(fmt.Errorf("largo de lectura inválido: %d", m.Len)), nil, false
		}
		data, err := s.fs.ReadAt(m.Path, m.Off, m.Len)
		if err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, data, true

	case "begin":
		w, err := s.fs.Begin(m.Path, m.Job, m.Size, m.UseBase)
		if err != nil {
			return errMsg(err), nil, false
		}
		s.hmu.Lock()
		s.next++
		id := s.next
		s.handles[id] = w
		s.hmu.Unlock()
		return Msg{H: id}, nil, false

	case "write":
		w, err := s.takeHandle(m.H)
		if err != nil {
			return errMsg(err), nil, false
		}
		if err := w.WriteAt(f.payload, m.Off); err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, nil, false

	case "commit":
		w, err := s.takeHandle(m.H)
		if err != nil {
			return errMsg(err), nil, false
		}
		s.dropHandle(m.H)
		sum, err := parseDigest(m.Hash)
		if err != nil {
			w.Abort()
			return errMsg(err), nil, false
		}
		if err := w.Commit(sum, m.Mode, m.MTime); err != nil {
			return errMsg(err), nil, false
		}
		return Msg{}, nil, false

	case "abort":
		if w, err := s.takeHandle(m.H); err == nil {
			s.dropHandle(m.H)
			w.Abort()
		}
		return Msg{}, nil, false
	}
	return errMsg(fmt.Errorf("operación desconocida: %q", m.Op)), nil, false
}
