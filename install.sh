#!/bin/sh
# Instala vextra: el binario único (mismo programa que "vx", su alias corto).
#
#   Modo remoto — descarga el binario para tu arquitectura desde un release
#   de GitHub y lo instala (requiere que REPO apunte a un release real; ver
#   .github/workflows/release.yml):
#     curl -fsSL https://raw.githubusercontent.com/REPO/HEAD/install.sh | REPO=usuario/vextra sh
#     curl -fsSL .../install.sh | REPO=usuario/vextra VERSION=v0.2.0 sh
#
#   Modo local — después de `make build` o `make dist`:
#     ./install.sh                    # usa ./vextra si existe
#     ./install.sh dist/vextra-linux-arm64
#
# Variables: PREFIX (/usr/local por defecto; usar $HOME/.local sin root),
# REPO (usuario/vextra; hace falta para el modo remoto), VERSION (latest por
# defecto), FORCE_VX=1 (pisa un "vx" de otro paquete instalado; no recomendado).
set -eu

PREFIX=${PREFIX:-/usr/local}
REPO=${REPO:-}
VERSION=${VERSION:-latest}

arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	armv7l | armv7 | armv6l) echo armv7 ;;
	*)
		echo "arquitectura no soportada: $(uname -m) (vextra V1 es sólo Linux amd64/arm64/armv7)" >&2
		exit 1
		;;
	esac
}

# fetch URL DESTINO — usa curl o wget, lo que haya. Devuelve el estado de la
# descarga tal cual (0 = ok), así que el llamador decide si es fatal o no.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -q "$1" -O "$2"
	else
		echo "hace falta curl o wget para instalar por esta vía" >&2
		exit 1
	fi
}

# download deja el binario verificado en $SRC (variable global, sin "local":
# más portable entre /bin/sh — dash, ash, bash en modo posix).
download() {
	[ "$(uname -s)" = "Linux" ] || {
		echo "vextra V1 sólo tiene binarios para Linux; en $(uname -s) compilá desde la fuente (make build)." >&2
		exit 1
	}
	if [ -z "$REPO" ]; then
		echo "no encuentro un binario local (./vextra) y no se definió REPO." >&2
		echo "  Uso local:  ./install.sh ruta/al/binario   (después de make build)" >&2
		echo "  Uso remoto: curl -fsSL .../install.sh | REPO=usuario/vextra sh" >&2
		exit 1
	fi
	a=$(arch) || exit 1
	base="https://github.com/$REPO/releases"
	if [ "$VERSION" = "latest" ]; then
		url_base="$base/latest/download"
	else
		url_base="$base/download/$VERSION"
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	echo "descargando vextra-linux-$a ($VERSION) de $REPO..."
	fetch "$url_base/vextra-linux-$a" "$tmp/vextra" ||
		{
			echo "no pude descargar $url_base/vextra-linux-$a — ¿existe ese release? (¿REPO y VERSION correctos?)" >&2
			exit 1
		}

	if fetch "$url_base/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null; then
		exp=$(grep "vextra-linux-$a\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
		got=$(sha256sum "$tmp/vextra" | cut -d' ' -f1)
		if [ -z "$exp" ]; then
			echo "aviso: SHA256SUMS no tiene una entrada para vextra-linux-$a; se instala sin verificar." >&2
		elif [ "$exp" != "$got" ]; then
			echo "el checksum no coincide (esperado $exp, descargado $got) — no se instala nada." >&2
			exit 1
		else
			echo "checksum verificado (sha256)."
		fi
	else
		echo "aviso: no encontré SHA256SUMS en el release; se instala sin verificar." >&2
	fi
	chmod +x "$tmp/vextra"
	SRC="$tmp/vextra"
}

SRC=${1:-}
if [ -z "$SRC" ]; then
	if [ -f ./vextra ]; then
		SRC=./vextra
	else
		download
	fi
elif [ ! -f "$SRC" ]; then
	echo "no encuentro $SRC" >&2
	exit 1
fi

install -d "$PREFIX/bin"
install -m 755 "$SRC" "$PREFIX/bin/vextra"
echo "instalado: $PREFIX/bin/vextra"

# Alias corto: sólo si "vx" está libre, o ya apunta a este mismo binario
# (una reinstalación no debe fallar ni duplicar el symlink).
existing=$(command -v vx 2>/dev/null || true)
if [ -z "$existing" ] || [ "$(readlink -f "$existing" 2>/dev/null || echo "$existing")" = "$(readlink -f "$PREFIX/bin/vextra")" ]; then
	ln -sf vextra "$PREFIX/bin/vx"
	echo "alias: $PREFIX/bin/vx -> vextra"
elif [ "${FORCE_VX:-0}" = "1" ]; then
	ln -sf vextra "$PREFIX/bin/vx"
	echo "alias forzado: $PREFIX/bin/vx -> vextra (pisa $existing)"
else
	echo "aviso: ya existe otro comando vx en $existing; no se toca."
	echo "       usá  vextra  (mismo programa), o reinstalá con FORCE_VX=1."
fi

case ":$PATH:" in
*":$PREFIX/bin:"*) ;;
*) echo "aviso: $PREFIX/bin no está en tu PATH todavía (abrí una terminal nueva, o agregalo a tu shell rc)." ;;
esac
echo "en el remoto sólo hace falta el binario vextra (vx install-remote host lo copia)."