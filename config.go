package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Config es la configuración efectiva. Valores por defecto razonables;
// un archivo opcional (~/.config/vextra/config.yaml, o --config / $VX_CONFIG)
// y los flags de línea de comandos los sobreescriben, en ese orden.
type Config struct {
	Compression string // auto | off
	Hash        string
	Resume      bool
	MaxDelete   int // 0 = ningún borrado permitido; <0 = sin límite
	ConfirmMass bool
	ConfirmOver int   // desde cuántos borrados se pide confirmación
	Workers     int   // 0 = auto
	MaxInflight int64 // bytes en vuelo entre todos los archivos
	BlockSize   int
	SmallFile   int64
	SSH         string
	RemoteBin   string
}

func DefaultConfig() Config {
	return Config{
		Compression: "auto",
		Hash:        hashName,
		Resume:      true,
		MaxDelete:   1000,
		ConfirmMass: true,
		ConfirmOver: 10,
		Workers:     0,
		MaxInflight: 256 << 20,
		BlockSize:   1 << 20,
		SmallFile:   64 << 10,
		SSH:         "ssh",
		RemoteBin:   "vextra",
	}
}

// EffectiveWorkers: auto = núcleos disponibles, con piso 4 (para tapar
// latencia de red con muchos archivos chicos) y techo 16.
func (c *Config) EffectiveWorkers() int {
	if c.Workers > 0 {
		return c.Workers
	}
	n := runtime.NumCPU()
	if n < 4 {
		n = 4
	}
	if n > 16 {
		n = 16
	}
	return n
}

func LoadConfig(explicit string) (*Config, error) {
	c := DefaultConfig()
	p := explicit
	if p == "" {
		p = os.Getenv("VX_CONFIG")
	}
	mustExist := p != ""
	if p == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			p = filepath.Join(dir, "vextra", "config.yaml")
		}
	}
	if p == "" {
		return &c, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !mustExist {
			return &c, nil
		}
		return nil, err
	}
	if err := c.parse(string(data)); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &c, nil
}

// parse entiende el YAML plano de la spec: "clave: valor" y un único nivel
// de anidamiento por indentación (safety:). Sin dependencias.
func (c *Config) parse(data string) error {
	section := ""
	for i, line := range strings.Split(data, "\n") {
		if j := strings.Index(line, "#"); j >= 0 {
			line = line[:j]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			return fmt.Errorf("línea %d: se esperaba \"clave: valor\"", i+1)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if !indented {
			section = ""
			if v == "" {
				section = k
				continue
			}
		}
		key := k
		if indented && section != "" {
			key = section + "." + k
		}
		if err := c.set(key, v); err != nil {
			return fmt.Errorf("línea %d: %w", i+1, err)
		}
	}
	return nil
}

func (c *Config) set(key, val string) error {
	var err error
	switch key {
	case "compression":
		if val != "auto" && val != "off" {
			return errors.New("compression: valores válidos: auto, off")
		}
		c.Compression = val
	case "hash":
		if val != hashName {
			return fmt.Errorf("hash %q no disponible en esta build (usa %s; BLAKE3 está en el backlog)", val, hashName)
		}
		c.Hash = val
	case "resume":
		c.Resume, err = strconv.ParseBool(val)
	case "safety.max_delete":
		c.MaxDelete, err = strconv.Atoi(val)
	case "safety.confirm_mass_delete":
		c.ConfirmMass, err = strconv.ParseBool(val)
	case "safety.confirm_over":
		c.ConfirmOver, err = strconv.Atoi(val)
	case "workers":
		if val == "auto" {
			c.Workers = 0
		} else if c.Workers, err = strconv.Atoi(val); err == nil && c.Workers < 1 {
			return errors.New("workers debe ser >= 1 o auto")
		}
	case "max_inflight":
		c.MaxInflight, err = parseSize(val)
	case "block_size":
		var v int64
		if v, err = parseSize(val); err == nil {
			if v < 4<<10 || v > 16<<20 {
				return errors.New("block_size debe estar entre 4KiB y 16MiB")
			}
			c.BlockSize = int(v)
		}
	case "small_file":
		c.SmallFile, err = parseSize(val)
	case "ssh":
		c.SSH = val
	case "remote_bin":
		c.RemoteBin = val
	default:
		return fmt.Errorf("clave desconocida: %s", key)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func (c *Config) String() string {
	w := "auto"
	if c.Workers > 0 {
		w = strconv.Itoa(c.Workers)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "compression: %s   # códec: %s\n", c.Compression, codecName)
	fmt.Fprintf(&b, "hash: %s\n", c.Hash)
	fmt.Fprintf(&b, "resume: %t\n", c.Resume)
	fmt.Fprintf(&b, "safety:\n")
	fmt.Fprintf(&b, "  max_delete: %d\n", c.MaxDelete)
	fmt.Fprintf(&b, "  confirm_mass_delete: %t\n", c.ConfirmMass)
	fmt.Fprintf(&b, "  confirm_over: %d\n", c.ConfirmOver)
	fmt.Fprintf(&b, "workers: %s   # efectivo: %d\n", w, c.EffectiveWorkers())
	fmt.Fprintf(&b, "max_inflight: %s\n", cfgSize(c.MaxInflight))
	fmt.Fprintf(&b, "block_size: %s\n", cfgSize(int64(c.BlockSize)))
	fmt.Fprintf(&b, "small_file: %s\n", cfgSize(c.SmallFile))
	fmt.Fprintf(&b, "ssh: %s\n", c.SSH)
	fmt.Fprintf(&b, "remote_bin: %s\n", c.RemoteBin)
	return b.String()
}
