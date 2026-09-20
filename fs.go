package main

import "errors"

// Tipos de entrada del sistema de archivos.
const (
	TFile = "f"
	TDir  = "d"
	TLink = "l"
)

// tmpMarker separa el nombre final del sufijo de temporal: <destino>.vxtmp.<job>
const tmpMarker = ".vxtmp."

// Errores tipados; main.go los mapea a códigos de salida estables.
var (
	ErrVerify      = errors.New("verificación de integridad fallida")
	ErrOutsideRoot = errors.New("ruta fuera de la raíz autorizada")
	ErrSafety      = errors.New("abortado por salvaguarda")
	ErrPartial     = errors.New("algunos archivos fallaron")
	ErrConn        = errors.New("error de conexión")
	ErrUsage       = errors.New("uso incorrecto")
)

// Entry describe un archivo, directorio o symlink. Path es relativo (con '/')
// a la raíz listada, o "." para la raíz misma.
type Entry struct {
	Path  string `json:"p"`
	Type  string `json:"t"`
	Size  int64  `json:"s,omitempty"`
	Mode  uint32 `json:"m"`  // sólo bits de permiso (0777)
	MTime int64  `json:"mt"` // unix nanosegundos
	Link  string `json:"l,omitempty"`
}

// Sigs son las firmas por bloque de un archivo (base del delta y de la reanudación).
type Sigs struct {
	Exists bool
	Kind   string // "final": el archivo destino; "tmp": temporal de una corrida previa
	Size   int64
	BS     int
	Hash   Digest   // hash del contenido completo
	Blocks []Digest // hash de cada bloque de BS bytes (el último puede ser menor)
}

// Writer escribe un archivo destino de forma atómica: todo va a un temporal
// y Commit verifica el hash completo antes del rename final.
type Writer interface {
	// WriteAt es seguro para uso concurrente.
	WriteAt(p []byte, off int64) error
	// Commit trunca al tamaño final, hace fsync, verifica el hash completo,
	// aplica permisos/mtime y renombra. Ante hash distinto borra el temporal
	// y devuelve un error que envuelve ErrVerify.
	Commit(sum Digest, mode uint32, mtime int64) error
	// Abort libera recursos conservando el temporal para poder reanudar.
	Abort()
}

// FS es un sistema de archivos (local, o remoto detrás del agente).
// Todas las rutas son POSIX. Las relativas se resuelven contra el cwd (local)
// o contra el home/raíz del agente (remoto).
type FS interface {
	// Stat devuelve la entrada de una ruta; follow=true sigue un symlink final.
	Stat(path string, follow bool) (*Entry, error)
	// List lista un directorio (recursivo o no). No sigue symlinks y omite temporales.
	List(path string, recursive bool) ([]Entry, error)
	Mkdir(path string, mode uint32) error // con padres (MkdirAll)
	Remove(path string, recursive bool) error
	Rename(from, to string) error
	Symlink(target, path string) error
	// SetMeta aplica permisos y, si mtime != 0, la fecha de modificación.
	SetMeta(path string, mode uint32, mtime int64) error
	// Sigs calcula firmas del archivo. Si job != "" y existe un temporal de
	// ese job, se firma el temporal (reanudación) en lugar del final.
	Sigs(path, job string, bs int) (*Sigs, error)
	ReadAt(path string, off int64, n int) ([]byte, error)
	// Begin abre (o reanuda) el temporal del job. useBase copia el archivo
	// final existente como punto de partida para aplicar un delta.
	Begin(path, job string, size int64, useBase bool) (Writer, error)
	Home() string
	Close() error
}
