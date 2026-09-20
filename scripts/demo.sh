#!/usr/bin/env bash
# Demo de Vextra: sync inicial, delta, reanudación y salvaguardas de borrado.
#
#   ./scripts/demo.sh                          # "remoto" simulado en esta máquina (sin sshd)
#   HOST=usuario@servidor ./scripts/demo.sh    # contra un servidor real por SSH
#
# Variables: BIN (ruta del binario), DEMO_DIR, REMOTE_DIR, BIG_MB (tamaño del archivo grande), CUT (segundos antes de cortar)
set -u
HERE=$(cd "$(dirname "$0")/.." && pwd)
BIN=${BIN:-$HERE/vextra}
[ -x "$BIN" ] || { echo "No encuentro $BIN. Compilá primero:  make build"; exit 1; }
export PATH="$(dirname "$BIN"):$PATH"

DEMO=${DEMO_DIR:-/tmp/vx-demo}
RDIR=${REMOTE_DIR:-/tmp/vx-demo-remote}
BIG_MB=${BIG_MB:-64}
CUT=${CUT:-2}

if [ -z "${HOST:-}" ]; then
  HOST=demo; SIM=1
  export VX_SSH="$HERE/scripts/fakessh.sh"
else
  SIM=0
fi

vx()   { "$BIN" "$@"; }
step() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
run()  { printf '\033[2m$ %s\033[0m\n' "$*"; "$@"; }
rrun() { if [ "$SIM" = 1 ]; then sh -c "$1"; else ssh "$HOST" "$1"; fi; }
rsha() { rrun "sha256sum '$1' | cut -d' ' -f1"; }

P="$DEMO/proyecto"
R="$HOST:$RDIR/proyecto"

step "0. Preparando datos de prueba ($([ $SIM = 1 ] && echo 'remoto simulado' || echo "remoto real: $HOST"))"
rm -rf "$DEMO"; rrun "rm -rf '$RDIR'"
mkdir -p "$P/src" "$P/docs"
for i in $(seq 1 60); do seq 1 300 > "$P/src/mod$i.txt"; done      # texto: comprime bien
head -c "${BIG_MB}M" /dev/urandom > "$P/datos.bin"                  # aleatorio: no comprime
echo "Proyecto de demo" > "$P/README.md"; ln -s ../README.md "$P/docs/leeme"
du -sh "$P" | sed 's/^/tamaño total: /'

step "1. diff: qué se copiaría (nada se modifica)"
run vx diff "$P" "$R" | tail -8

step "2. sync inicial"
run vx sync "$P" "$R"

step "3. verificación independiente"
if [ "$SIM" = 1 ]; then
  diff -r "$P" "$RDIR/proyecto" && echo "diff -r: idénticos"
fi
[ "$(sha256sum "$P/datos.bin" | cut -d' ' -f1)" = "$(rsha "$RDIR/proyecto/datos.bin")" ] && echo "sha256 de datos.bin coincide en ambos lados"
run vx diff --exit-code "$P" "$R"; echo "código de salida: $? (0 = sin diferencias)"

step "4. DELTA: cambio 1 MiB dentro del archivo de ${BIG_MB} MiB y toco un archivo de texto"
dd if=/dev/urandom of="$P/datos.bin" bs=1M count=1 seek=$((BIG_MB / 3)) conv=notrunc 2>/dev/null
echo "linea nueva" >> "$P/src/mod3.txt"
run vx diff "$P" "$R"
run vx sync -v "$P" "$R"
[ "$(sha256sum "$P/datos.bin" | cut -d' ' -f1)" = "$(rsha "$RDIR/proyecto/datos.bin")" ] && echo "sha256 coincide tras el delta"

step "5. REANUDACIÓN: copio un archivo de 96 MiB y corto el proceso a los ${CUT}s"
head -c 96M /dev/urandom > "$P/grande.bin"
timeout -s KILL "$CUT" "$BIN" put --bwlimit 30MiB "$P/grande.bin" "$R/grande.bin"; echo "(proceso cortado, código $?)"
echo "temporal que quedó en el destino:"; rrun "ls -la '$RDIR/proyecto' | grep vxtmp || echo '  (no llegó a crearse; probá con CUT=3)'"
echo "destino final todavía no existe:"; rrun "ls '$RDIR/proyecto/grande.bin' 2>&1 | head -1"
run vx put "$P/grande.bin" "$R/grande.bin"
[ "$(sha256sum "$P/grande.bin" | cut -d' ' -f1)" = "$(rsha "$RDIR/proyecto/grande.bin")" ] && echo "sha256 coincide tras reanudar"

step "6. SALVAGUARDAS: borro 15 archivos en origen y sincronizo con --delete"
rm -f "$P"/src/mod{1..15}.txt
echo "-- sin --delete: los sobrantes sólo se informan"
run vx diff "$P" "$R" | tail -3
echo "-- --delete con --max-delete 5: debe abortar sin tocar nada"
run vx sync --delete --max-delete 5 "$P" "$R"; echo "código de salida: $? (5 = salvaguarda)"
echo "-- --delete sin confirmación posible (stdin no interactivo): debe abortar"
run vx sync --delete "$P" "$R" < /dev/null; echo "código de salida: $? (5 = salvaguarda)"
echo "-- --delete --yes: se aplica"
run vx sync --delete --yes "$P" "$R"
run vx diff --exit-code "$P" "$R"; echo "código de salida: $?"

step "7. configuración efectiva"
run vx config show
echo; echo "Demo terminada. Datos en $DEMO (y $RDIR en el remoto)."
