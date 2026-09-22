#!/usr/bin/env bash
# Benchmark de Vextra vs rsync y scp, sobre los mismos datos y el mismo
# "transporte": un ssh real si pasás HOST, o un ssh simulado en esta misma
# máquina si no (mismo mecanismo que scripts/demo.sh; ver ADVERTENCIA abajo).
#
#   ./scripts/bench.sh                          # transporte simulado, sin red real
#   HOST=usuario@servidor ./scripts/bench.sh     # contra un servidor real por SSH
#
# Variables: BIN, DATA_DIR, REMOTE_DIR, SMALL_N (archivos chicos), BIG_MB
# (archivo grande incompresible), EDIT_MB (cuánto se edita en el escenario delta).
#
# ADVERTENCIA: con el transporte simulado no hay latencia ni ancho de banda
# real de red — los tiempos no son comparables a una corrida contra un
# servidor de verdad, sólo sirven para comparar el trabajo que hace cada
# herramienta (CPU, E/S, protocolo). Para un número que valga como benchmark,
# corré esto con HOST=usuario@servidor contra una red real.
set -u
HERE=$(cd "$(dirname "$0")/.." && pwd)
BIN=${BIN:-$HERE/vextra}
[ -x "$BIN" ] || { echo "No encuentro $BIN. Compilá primero:  make build" >&2; exit 1; }

DATA=${DATA_DIR:-/tmp/vx-bench}
RDIR=${REMOTE_DIR:-/tmp/vx-bench-remote}
SMALL_N=${SMALL_N:-500}
BIG_MB=${BIG_MB:-256}
EDIT_MB=${EDIT_MB:-1}

if [ -z "${HOST:-}" ]; then
  HOST=bench; SIM=1; RSH="$HERE/scripts/fakessh.sh"
else
  SIM=0; RSH="ssh"
fi
export VX_SSH="$RSH"

have()  { command -v "$1" >/dev/null 2>&1; }
now()   { date +%s.%N; }
secs()  { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f", b-a}'; }
rrun()  { if [ "$SIM" = 1 ]; then sh -c "$1"; else ssh "$HOST" "$1"; fi; }
human() { awk -v n="$1" 'BEGIN{split("B KiB MiB GiB TiB",u," ");i=1
  while(n>=1024&&i<5){n/=1024;i++}; if(i==1)printf "%d %s",n,u[i]; else printf "%.1f %s",n,u[i]}'; }

# wire_bytes extrae "bytes en el cable" de la salida de cada herramienta.
# vx: su propia línea de resumen. rsync: enviados+recibidos de --stats.
# scp no imprime esto (siempre copia completo; ver nota al pie).
wire_bytes() {
  case "$1" in
  vx)
    echo "$2" | grep -oE 'En el cable[^:]*: [0-9.]+ ?[A-Za-z]*' | grep -oE '[0-9.]+ ?[A-Za-z]*$'
    ;;
  rsync)
    sent=$(echo "$2" | grep -oE 'Total bytes sent: [0-9,]+' | grep -oE '[0-9,]+' | tr -d ,)
    recv=$(echo "$2" | grep -oE 'Total bytes received: [0-9,]+' | grep -oE '[0-9,]+' | tr -d ,)
    [ -n "$sent" ] && [ -n "$recv" ] && human $((sent + recv)) || echo "?"
    ;;
  esac
}

declare -a ROWS

# record corre un comando, mide tiempo real y guarda una fila del reporte.
# Uso: record "escenario" "herramienta" comando...
record() {
  local scen="$1" tool="$2"; shift 2
  local t0 t1 out rc
  t0=$(now)
  out=$("$@" 2>&1); rc=$?
  t1=$(now)
  if [ $rc -ne 0 ]; then
    ROWS+=("$(printf '%-16s %-6s %8s  %-14s  falló (código %d)' "$scen" "$tool" "-" "-" "$rc")")
    echo "  [$tool] salida:" >&2; echo "$out" | sed 's/^/    /' >&2
    return
  fi
  local t b
  t=$(secs "$t0" "$t1")
  b=$(wire_bytes "$tool" "$out")
  [ -z "$b" ] && b="n/d"
  ROWS+=("$(printf '%-16s %-6s %7ss  %s' "$scen" "$tool" "$t" "$b")")
}

echo "== Preparando datos: ${SMALL_N} archivos de texto + ${BIG_MB}MiB incompresibles =="
rm -rf "$DATA"; rrun "rm -rf '$RDIR'" >/dev/null 2>&1
mkdir -p "$DATA/src"
for i in $(seq 1 "$SMALL_N"); do seq 1 200 > "$DATA/src/f$i.txt"; done
head -c "${BIG_MB}M" /dev/urandom > "$DATA/src/big.bin"
du -sh "$DATA/src" | sed 's/^/   tamaño total: /'
echo

echo "-- A) copia inicial en frío --"
rrun "rm -rf '$RDIR/dst-vx' '$RDIR/dst-rsync' '$RDIR/dst-scp'"
record "copia inicial" vx "$BIN" sync "$DATA/src" "$HOST:$RDIR/dst-vx"
have rsync && record "copia inicial" rsync rsync -a --stats -e "$RSH" "$DATA/src/" "$HOST:$RDIR/dst-rsync/"
have scp && record "copia inicial" scp scp -rq -S "$RSH" "$DATA/src" "$HOST:$RDIR/dst-scp"

echo "-- B) re-sync sin cambios (scp no tiene este concepto: siempre copia todo) --"
record "sin cambios" vx "$BIN" sync "$DATA/src" "$HOST:$RDIR/dst-vx"
have rsync && record "sin cambios" rsync rsync -a --stats -e "$RSH" "$DATA/src/" "$HOST:$RDIR/dst-rsync/"

echo "-- C) delta: se edita ~${EDIT_MB}MiB de ${BIG_MB}MiB (≈$((100*EDIT_MB/BIG_MB))%) --"
seek=$(( (BIG_MB - EDIT_MB) / 2 )); [ "$seek" -lt 0 ] && seek=0
dd if=/dev/urandom of="$DATA/src/big.bin" bs=1M count="$EDIT_MB" seek="$seek" conv=notrunc 2>/dev/null
record "delta ~1%" vx "$BIN" sync "$DATA/src" "$HOST:$RDIR/dst-vx"
have rsync && record "delta ~1%" rsync rsync -a --stats -e "$RSH" "$DATA/src/" "$HOST:$RDIR/dst-rsync/"
have scp && record "delta ~1%" scp scp -rq -S "$RSH" "$DATA/src" "$HOST:$RDIR/dst-scp"

echo
printf '%-16s %-6s %9s  %s\n' "ESCENARIO" "HERR." "TIEMPO" "EN EL CABLE"
printf '%-16s %-6s %9s  %s\n' "----------------" "------" "--------" "-----------"
for r in "${ROWS[@]}"; do echo "$r"; done
echo
echo "vx: línea \"En el cable\" de su propio resumen (incluye protocolo y compresión)."
echo "rsync: bytes enviados + recibidos de --stats (mismo significado: tráfico real del protocolo)."
if ! have scp; then echo "scp: no instalado, se omitió."; fi
echo "scp no tiene modo delta ni --stats: siempre transfiere el árbol completo (por eso no aparece en 'sin cambios')."
[ "$SIM" = 1 ] && echo "Transporte simulado (sin red real): los tiempos comparan trabajo, no ancho de banda. Repetí con HOST=usuario@servidor para un número real."