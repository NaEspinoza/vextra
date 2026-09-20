package main

// Protocolo de agente de Vextra (V1).
//
// Viaja por stdin/stdout de un `ssh host vextra agent`. Es deliberadamente
// simple y depurable: frames de longitud prefijada con cabecera JSON y un
// payload binario opcional. Cada request lleva un id; el agente responde
// con el mismo id, en cualquier orden, por lo que varias transferencias
// pueden estar en vuelo a la vez sobre un único canal SSH.
//
//	frame   = len(u32) | id(u32) | flags(u8) | hlen(u32) | header(JSON) | payload
//	len     = bytes que siguen a este campo (9 + hlen + len(payload))
//	flags   = bit0: respuesta · bit1: payload comprimido (Msg.RawLen = largo original)

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"
)

const (
	protoVersion = 1
	maxFrame     = 256 << 20 // techo duro por frame (256 MiB)
	flagResp     = 1 << 0
	flagZ        = 1 << 1
)

// agentMagic lo imprime el agente al arrancar. El cliente descarta todo lo
// que llegue antes (banners de shell, motd) hasta encontrarlo.
const agentMagic = "\x00VEXTRA-AGENT/1\n"

// Msg es la cabecera JSON de todos los mensajes (request y respuesta).
type Msg struct {
	Op        string `json:"op,omitempty"`
	Path      string `json:"path,omitempty"`
	To        string `json:"to,omitempty"`
	Job       string `json:"job,omitempty"`
	Recursive bool   `json:"rec,omitempty"`
	Follow    bool   `json:"follow,omitempty"`
	UseBase   bool   `json:"base,omitempty"`
	Z         bool   `json:"z,omitempty"` // el cliente acepta payload de respuesta comprimido
	Mode      uint32 `json:"mode,omitempty"`
	MTime     int64  `json:"mtime,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Off       int64  `json:"off,omitempty"`
	Len       int    `json:"len,omitempty"`
	BS        int    `json:"bs,omitempty"`
	H         uint32 `json:"h,omitempty"` // handle de escritura
	Hash      string `json:"hash,omitempty"`
	RawLen    int    `json:"raw,omitempty"`

	// Respuesta
	Err    string `json:"err,omitempty"`
	Code   string `json:"code,omitempty"`
	Exists bool   `json:"exists,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Ver    int    `json:"ver,omitempty"`
	Home   string `json:"home,omitempty"`
	Host   string `json:"host,omitempty"`
	Arch   string `json:"arch,omitempty"`
	Entry  *Entry `json:"entry,omitempty"`
}

type frame struct {
	id      uint32
	flags   byte
	msg     Msg
	payload []byte // ya descomprimido
	wire    int    // bytes que ocupó en el cable
}

func writeFrame(w io.Writer, id uint32, flags byte, m *Msg, payload []byte) (int, error) {
	hdr, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	total := 9 + len(hdr) + len(payload)
	if total > maxFrame {
		return 0, fmt.Errorf("frame demasiado grande (%d bytes)", total)
	}
	head := make([]byte, 13+len(hdr))
	binary.BigEndian.PutUint32(head[0:4], uint32(total))
	binary.BigEndian.PutUint32(head[4:8], id)
	head[8] = flags
	binary.BigEndian.PutUint32(head[9:13], uint32(len(hdr)))
	copy(head[13:], hdr)
	n, err := w.Write(head)
	if err != nil {
		return n, err
	}
	if len(payload) > 0 {
		n2, err := w.Write(payload)
		n += n2
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func readFrame(r io.Reader) (*frame, error) {
	var pre [4]byte
	if _, err := io.ReadFull(r, pre[:]); err != nil {
		return nil, err
	}
	total := binary.BigEndian.Uint32(pre[:])
	if total < 9 || total > maxFrame {
		return nil, fmt.Errorf("frame inválido (len=%d)", total)
	}
	body := make([]byte, total)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	f := &frame{
		id:    binary.BigEndian.Uint32(body[0:4]),
		flags: body[4],
		wire:  int(total) + 4,
	}
	hl := binary.BigEndian.Uint32(body[5:9])
	if uint64(hl)+9 > uint64(total) {
		return nil, errors.New("cabecera de frame inválida")
	}
	if err := json.Unmarshal(body[9:9+hl], &f.msg); err != nil {
		return nil, fmt.Errorf("cabecera JSON inválida: %w", err)
	}
	f.payload = body[9+hl:]
	if f.flags&flagZ != 0 {
		p, err := decompressBlock(f.payload, f.msg.RawLen)
		if err != nil {
			return nil, err
		}
		f.payload = p
	}
	return f, nil
}

// RemoteError es un error devuelto por el agente. Conserva el código para
// que errors.Is funcione igual que con errores locales (ENOENT, EVERIFY...).
type RemoteError struct{ Code, Text string }

func (e *RemoteError) Error() string { return e.Text }

func (e *RemoteError) Is(target error) bool {
	switch e.Code {
	case "ENOENT":
		return target == fs.ErrNotExist
	case "EVERIFY":
		return target == ErrVerify
	case "EROOT":
		return target == ErrOutsideRoot
	}
	return false
}

func codeFor(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, ErrVerify):
		return "EVERIFY"
	case errors.Is(err, ErrOutsideRoot):
		return "EROOT"
	}
	return "EIO"
}

// Conn multiplexa requests concurrentes sobre un único par de streams.
type Conn struct {
	w       io.Writer
	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[uint32]chan *frame
	next    uint32
	err     error
	closeFn func() error
	done    chan struct{} // se cierra cuando termina el loop de lectura

	BytesIn  atomic.Int64
	BytesOut atomic.Int64
}

var errClosed = errors.New("conexión cerrada")

func NewConn(r io.Reader, w io.Writer, closeFn func() error) *Conn {
	c := &Conn{w: w, pending: map[uint32]chan *frame{}, closeFn: closeFn, done: make(chan struct{})}
	go c.readLoop(r)
	return c
}

func (c *Conn) readLoop(r io.Reader) {
	defer close(c.done)
	for {
		f, err := readFrame(r)
		if err != nil {
			c.fail(err)
			return
		}
		c.BytesIn.Add(int64(f.wire))
		c.mu.Lock()
		ch := c.pending[f.id]
		delete(c.pending, f.id)
		c.mu.Unlock()
		if ch != nil {
			ch <- f // buffer de 1: nunca bloquea
		}
	}
}

// fail marca la conexión como caída y despierta a todos los que esperan.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		c.err = fmt.Errorf("%w: %v", ErrConn, err)
	}
	pend := c.pending
	c.pending = map[uint32]chan *frame{}
	c.mu.Unlock()
	for _, ch := range pend {
		close(ch)
	}
}

func (c *Conn) connErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return ErrConn
}

func (c *Conn) send(id uint32, flags byte, m *Msg, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n, err := writeFrame(c.w, id, flags, m, payload)
	c.BytesOut.Add(int64(n))
	return err
}

// Call envía un request y espera su respuesta. Es seguro llamarlo desde
// muchas goroutines a la vez. Si z es true, payload ya viene comprimido y
// m.RawLen debe traer su largo original.
func (c *Conn) Call(m Msg, payload []byte, z bool) (*frame, error) {
	c.mu.Lock()
	if c.err != nil {
		e := c.err
		c.mu.Unlock()
		return nil, e
	}
	c.next++
	id := c.next
	ch := make(chan *frame, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	flags := byte(0)
	if z {
		flags |= flagZ
	}
	if err := c.send(id, flags, &m, payload); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.fail(err)
		return nil, c.connErr()
	}
	f, ok := <-ch
	if !ok {
		return nil, c.connErr()
	}
	if f.msg.Err != "" {
		return nil, &RemoteError{Code: f.msg.Code, Text: f.msg.Err}
	}
	return f, nil
}

func (c *Conn) Close() error {
	c.fail(errClosed)
	if c.closeFn != nil {
		return c.closeFn()
	}
	return nil
}
