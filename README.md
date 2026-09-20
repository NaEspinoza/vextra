# Vextra (V1 demo)

Transferencia y sincronización de archivos sobre SSH para Linux: un binario estático, sin dependencias
(solo stdlib de Go). Comando: **`vextra`**, con alias corto **`vx`** (el instalador no pisa un `vx` ajeno;
en el remoto el agente siempre se invoca como `vextra`, así que un `vx` de otro paquete nunca interfiere).

> **Estado de verificación:** este código se escribió en un entorno sin compilador Go ni red, así que
> **no fue compilado ni ejecutado todavía**. Se revisó a mano y con un verificador estático (imports,
> variables sin uso, brackets, aridad de llamadas). El primer paso es `make test` (ver abajo); si algo no
> compila o falla un test, pegá la salida y se corrige.

## Compilar, probar, demo

```sh
go version            # Go >= 1.21
make test             # go test ./...  (unitarios + integración con agente en proceso, sin ssh)
make build            # ./vextra  (estático, CGO_ENABLED=0)
make demo             # sync, delta, reanudación y salvaguardas; "remoto" simulado en tu máquina
HOST=usuario@servidor ./scripts/demo.sh    # la misma demo contra un servidor real
make dist             # dist/vextra-linux-{amd64,arm64,armv7}
sudo make install     # /usr/local/bin/vextra (+ alias vx si el nombre está libre)
```

Contra un servidor real, el remoto necesita el binario `vextra` (mismo mecanismo que rsync):
`vx install-remote usuario@servidor` lo copia a `~/.local/bin/vextra` (misma arquitectura; si no, usá `make dist`).

## Uso

```sh
vx put  ./proyecto host:/srv/proyecto      # local -> remoto
vx get  host:/srv/proyecto ./copia         # remoto -> local
vx sync ./proyecto host:/srv/proyecto      # espejo (también remoto -> local, o local -> local)
vx diff ./proyecto host:/srv/proyecto      # qué cambiaría (dry-run); --exit-code => código 10 si hay diferencias
vx sync --delete --max-delete 50 ./a host:/b   # borra sobrantes en destino, con límite y confirmación
vx connect usuario@host                    # shell: ls cd pwd mkdir rm mv | lls lcd lpwd lmkdir lrm lmv | get put sync diff
vx config show
```

Semántica de rutas (idéntica en put/get/sync/diff): un **directorio** origen se fusiona *en* el destino
(sin anidar: repetir el comando es idempotente); un **archivo** origen va a la ruta destino, o adentro si
esa ruta es un directorio existente. Opciones: `vx help`. Configuración opcional en
`~/.config/vextra/config.yaml` (formato de la spec §13). SSH: `--ssh "ssh -p 2222 -i key"`, `$VX_SSH` o clave `ssh:`.

Códigos de salida: 0 ok · 1 error · 2 uso · 3 conexión · 4 integridad · 5 salvaguarda · 6 parcial · 10 diff.

## Cómo funciona

```
CLI/Shell ─► Sync Engine ─► FS origen ─┐                    (un FS = local, o remoto tras el agente)
             plan/quick-check           ├─► Transfer: bloques de 1 MiB, hash, workers acotados
             delta/reanudación         FS destino ◄┘         escritura atómica en <destino>.vxtmp.<job>
Transporte: `ssh host vextra agent` — frames [len|id|flags|JSON|payload] multiplexados por stdin/stdout
```

- **Quick-check** por tipo/tamaño/mtime/modo. Si sólo cambió el mtime, se comparan firmas por bloque y, si el contenido es igual, se actualizan sólo los metadatos.
- **Delta**: firmas SHA-256 por bloque fijo a ambos lados; sólo viajan los bloques distintos (comparación posicional).
- **Reanudación**: el temporal tiene nombre determinístico (destino+tamaño+mtime); al reintentar, ese temporal parcial se usa como base del delta, por lo que sirve cualquier subconjunto de bloques ya escritos.
- **Integridad/atomicidad**: `fsync` + hash completo verificado **antes** del `rename`; ante hash distinto se borra el temporal y se sale con código 4. El destino nunca queda a medias.
- **Concurrencia**: N archivos en paralelo, bloques concurrentes por archivo, todo limitado por un presupuesto de bytes en vuelo (`max_inflight`) y `--bwlimit` opcional.
- **Compresión adaptativa** por bloque: se omite en formatos ya comprimidos y tras 2 intentos sin ahorro real en un archivo.
- **Seguridad**: los listados remotos se validan (sin `..`, sin entradas dentro de symlinks) para que un servidor hostil no pueda escribir fuera del destino; `--remote-root` confina al agente (traversal y symlinks que escapan). Con `--delete`: `--max-delete`, confirmación desde 10 borrados, y no se borra nada si hubo errores.

## Cobertura de la spec y desvíos deliberados

Cubierto: §3 completo salvo lo indicado abajo, §5 (shell y modo comando), §6, §7, §9, §11.2–11.4, §13, fases 0–2 y buena parte de 3–4.

| Spec | En esta build | Cómo cerrarlo |
|---|---|---|
| BLAKE3 | SHA-256 (stdlib) | cambiar `newHash()` en `hash.go` (2 líneas) |
| zstd | DEFLATE nivel 1 (stdlib) | cambiar `compressBlock/decompressBlock` en `codec.go` |
| `x/crypto/ssh` + SFTP | binario `ssh` del sistema + agente propio | ver abajo |
| `sync` = espejo | **no borra por defecto**; espejo exacto con `--delete` | decisión de seguridad (aws-s3-sync/rsync); cambiar el default es 1 línea |
| `diff` exit code | 0 salvo `--exit-code` (10) | — |

Sobre el transporte: usar `ssh` del sistema hereda gratis `~/.ssh/config`, ProxyJump, ssh-agent, known_hosts y
ControlMaster, y se cumple el objetivo de "no reinventar auth". El costo: el **cliente** necesita OpenSSH, y el
**remoto** necesita el binario `vextra` (no se usa SFTP: el delta requiere calcular firmas en el remoto).
Un transporte nativo con `x/crypto/ssh` entra detrás de la misma interfaz (`io.Reader/Writer`); un modo de
respaldo SFTP puro para hosts sin binario es backlog.

## Límites conocidos (V1)

Delta posicional (una inserción desplaza el resto y se reenvía; FastCDC está en el backlog de la spec) · los
listados se arman en memoria (~100 B/entrada) · en archivos modificados se lee el archivo completo a ambos lados
en cada corrida (SHA-256 sin rolling) · no se preservan owner/xattrs/hardlinks/mtime de directorios; sockets y
dispositivos se ignoran · un directorio ilegible aborta la corrida (a propósito: con `--delete` no puede
confundirse con "vacío") · el shell no tiene edición de línea ni historial (usar `rlwrap vx connect ...`) ·
nombres de archivo de ~230+ bytes no admiten el sufijo del temporal.

## Próximos pasos sugeridos

1. Compilar y pasar `make test`; correr `make demo`. 2. Benchmarks reales contra `rsync`/`scp` (fase 3). 3. BLAKE3 + zstd.
4. `--json` para automatización. 5. Transporte `x/crypto/ssh` nativo y/o fallback SFTP.
