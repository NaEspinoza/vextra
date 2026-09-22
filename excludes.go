package main

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strings"
)

// excludeSet son patrones al estilo rsync — deliberadamente un subconjunto
// simple, sin "**" ni reglas de inclusión/negación (eso es backlog si hace
// falta). La sintaxis de cada patrón es la de path.Match (*, ?, [clase]);
// "*" nunca cruza un "/".
//
//	sin "/"        coincide con el nombre, en cualquier profundidad   (*.log, node_modules, .git)
//	con "/"        se ancla a la ruta relativa completa desde la raíz  (cache/tmp, /solo-en-la-raiz)
//	"/" al final   sólo coincide con directorios (y por lo tanto con todo lo que hay adentro)
//
// Una entrada se excluye si ella misma matchea, o si CUALQUIER ancestro suyo
// matchea un patrón de directorio: excluir "node_modules/" excluye también
// "node_modules/pkg/index.js" aunque el patrón nunca mencione ese archivo.
type excludeSet struct {
	patterns []pattern
}

type pattern struct {
	raw      string
	anchored bool
	dirOnly  bool
	body     string
}

// compileExcludes valida y compila los patrones. Las líneas vacías y las que
// empiezan con "#" se ignoran (así --exclude-from acepta comentarios).
func compileExcludes(raw []string) (*excludeSet, error) {
	es := &excludeSet{}
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" || strings.HasPrefix(r, "#") {
			continue
		}
		p := pattern{raw: r, dirOnly: strings.HasSuffix(r, "/")}
		body := strings.TrimSuffix(r, "/")
		p.anchored = strings.Contains(body, "/")
		body = strings.TrimPrefix(body, "/")
		if body == "" {
			return nil, fmt.Errorf("patrón de exclusión vacío: %q", r)
		}
		if _, err := path.Match(body, "x"); err != nil {
			return nil, fmt.Errorf("patrón de exclusión inválido %q: %w", r, err)
		}
		p.body = body
		es.patterns = append(es.patterns, p)
	}
	return es, nil
}

// loadExcludeFile lee un patrón por línea (formato de --exclude-from).
func loadExcludeFile(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out, sc.Err()
}

func (es *excludeSet) empty() bool { return es == nil || len(es.patterns) == 0 }

// match evalúa un único patrón contra un segmento de una ruta ya partida:
// full es la ruta relativa acumulada hasta ese segmento (para patrones
// anclados), base es el nombre de ese segmento (para patrones sin "/"), y
// dirLike indica si ese segmento es, o contiene, un directorio.
func (pt pattern) match(full, base string, dirLike bool) bool {
	if pt.dirOnly && !dirLike {
		return false
	}
	target := base
	if pt.anchored {
		target = full
	}
	ok, _ := path.Match(pt.body, target) // el patrón ya se validó al compilar
	return ok
}

// Excluded evalúa una entrada (identificada por su ruta relativa y tipo)
// contra el conjunto: alcanza con que ella misma, o cualquiera de sus
// ancestros, matchee un patrón.
func (es *excludeSet) Excluded(rel string, typ string) bool {
	if es.empty() {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		full := strings.Join(parts[:i+1], "/")
		last := i == len(parts)-1
		dirLike := !last || typ == TDir
		for _, pt := range es.patterns {
			if pt.match(full, parts[i], dirLike) {
				return true
			}
		}
	}
	return false
}

// filterExcluded devuelve list sin las entradas que matchean es (directas o
// por herencia de un ancestro excluido). No modifica list.
func filterExcluded(list []Entry, es *excludeSet) []Entry {
	if es.empty() {
		return list
	}
	out := make([]Entry, 0, len(list))
	for i := range list {
		if !es.Excluded(list[i].Path, list[i].Type) {
			out = append(out, list[i])
		}
	}
	return out
}