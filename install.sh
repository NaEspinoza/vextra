#!/bin/sh
# Instala vextra y, si el nombre está libre, el alias corto "vx".
#   ./install.sh [ruta-al-binario]      PREFIX=/usr/local por defecto (usar PREFIX=$HOME/.local sin root)
#   FORCE_VX=1 ./install.sh             pisa un "vx" ajeno (no recomendado)
set -eu
PREFIX=${PREFIX:-/usr/local}
SRC=${1:-./vextra}
[ -f "$SRC" ] || { echo "no encuentro $SRC (compilá con: make build)"; exit 1; }
install -d "$PREFIX/bin"
install -m 755 "$SRC" "$PREFIX/bin/vextra"
echo "instalado: $PREFIX/bin/vextra"
existing=$(command -v vx 2>/dev/null || true)
if [ -z "$existing" ] || [ "$(readlink -f "$existing")" = "$(readlink -f "$PREFIX/bin/vextra")" ]; then
  ln -sf vextra "$PREFIX/bin/vx"
  echo "alias: $PREFIX/bin/vx -> vextra"
elif [ "${FORCE_VX:-0}" = "1" ]; then
  ln -sf vextra "$PREFIX/bin/vx"
  echo "alias forzado: $PREFIX/bin/vx -> vextra (pisa $existing)"
else
  echo "aviso: ya existe otro comando vx en $existing; no se toca."
  echo "       usá  vextra  (mismo programa), o reinstalá con FORCE_VX=1."
fi
echo "en el remoto sólo hace falta el binario vextra (vx install-remote host lo copia)."
