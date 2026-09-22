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
make bench             # vx vs rsync vs scp: copia en frío, sin cambios, delta ~1% (ver "Rendimiento" abajo)
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

## Conexión SSH: puerto, clave, bastión, o pegar un comando ssh

Todo va por el `ssh` del sistema, así que cualquiera de estas formas sirve en `connect`, `put`, `get`, `sync`, `diff` e `install-remote`:

```sh
# 1) Flags (mismos nombres que ssh)
vx connect -p 2222 -i ~/.ssh/id_ed25519 usuario@host
vx sync -p 2222 -i ~/.ssh/k -J bastion -o StrictHostKeyChecking=accept-new ./dir host:/srv/dir

# 2) Pegar un comando ssh tal cual (entre comillas si trae opciones que vx no define)
vx connect "ssh -p 2222 -i ~/.ssh/k -J bastion usuario@host"

# 3) Alias de ~/.ssh/config (la opción más cómoda para el uso diario)
#    Host prod / HostName 10.0.0.5 / Port 2222 / User deploy / IdentityFile ~/.ssh/prod
vx connect prod
vx sync ./dir prod:/srv/dir

# 4) Un comando ssh por defecto: --ssh, $VX_SSH, o `ssh:` en ~/.config/vextra/config.yaml
export VX_SSH="ssh -p 2222 -i /home/yo/.ssh/k"
```

**Precedencia.** OpenSSH se queda con el *primer* valor que ve de cada opción, así que vextra arma el comando en este
orden: primero los flags de la línea de comandos, después las opciones de un comando pegado y al final el comando por
defecto (`--ssh`, `$VX_SSH`, configuración). Lo que escribís en el momento siempre gana; por ejemplo, con
`VX_SSH="ssh -p 22"`, un `--port 2222` se respeta. (`-J` es la excepción: ssh no admite dos, así que no lo repitas
entre el comando por defecto y un flag.)

Contraseñas y passphrases: `ssh` las pide directamente en tu terminal (no pasan por vextra); `ssh-agent` funciona igual.
Al pegar un comando ssh se descartan `-t -T -N -f -n` (romperían el agente) y se rechaza si trae un comando remoto al final.

## Cómo funciona

```
CLI/Shell ─► Sync Engine ─► FS origen ─┐                    (un FS = local, o remoto tras el agente)
             plan/quick-check           ├─► Transfer: bloque adaptativo, hash, workers acotados
             delta/reanudación         FS destino ◄┘         escritura atómica en <destino>.vxtmp.<job>
Transporte: `ssh host vextra agent` — frames [len|id|flags|JSON|payload] multiplexados por stdin/stdout
```

- **Quick-check** por tipo/tamaño/mtime/modo. Si sólo cambió el mtime, se comparan firmas por bloque y, si el contenido es igual, se actualizan sólo los metadatos. En directorios también se compara el mtime (no sólo el modo).
- **Delta**: firmas SHA-256 por bloque a ambos lados; sólo viajan los bloques distintos (comparación posicional). El tamaño de bloque es **adaptativo**: apunta a ~512 bloques por archivo (entre 64 KiB y 4 MiB), en vez de un 1 MiB fijo. Esto importa para el criterio de la fase 2 ("editar 1% transfiere mucho menos que el archivo completo"): con un bloque fijo grande, el 1% de un archivo chico igual puede tocar un porcentaje alto del archivo; con el tamaño adaptativo, esa proporción se mantiene chica sea cual sea el tamaño del archivo. `--block-size` sigue disponible para fijar un valor (por ejemplo, para que dos corridas sean bit-a-bit comparables).
- **`diff --deep`**: sin `--deep`, `diff` muestra una cota superior (todo el archivo cambiado cuenta entero). Con `--deep`, calcula el delta real: lee las firmas de ambos lados —en paralelo, cada lado calcula las suyas donde vive el archivo— y muestra cuántos bloques y bytes se moverían de verdad. Es el mismo cálculo que hace la transferencia real, así que el número coincide (ver `TestDeepDiffMatchesRealTransfer`).
- **Reanudación**: el temporal tiene nombre determinístico (destino+tamaño+mtime); al reintentar, ese temporal parcial se usa como base del delta, por lo que sirve cualquier subconjunto de bloques ya escritos.
- **Integridad/atomicidad**: `fsync` + hash completo verificado **antes** del `rename`; ante hash distinto se borra el temporal y se sale con código 4. El destino nunca queda a medias.
- **Concurrencia**: N archivos en paralelo, bloques concurrentes por archivo, todo limitado por un presupuesto de bytes en vuelo (`max_inflight`) y `--bwlimit` opcional.
- **Compresión adaptativa** por bloque: se omite en formatos ya comprimidos y tras 2 intentos sin ahorro real en un archivo.
- **Exclusiones** (`--exclude`, `--exclude-from`, `exclude:` en la configuración): patrones al estilo rsync; ver la sección "Exclusiones" más abajo.
- **Metadatos de directorios**: permisos y mtime se propagan y se fijan **al final** de la corrida (crear o borrar algo adentro de un directorio le cambia el mtime, así que tocarlo antes se perdería).
- **Seguridad**: los listados remotos se validan (sin `..`, sin entradas dentro de symlinks) para que un servidor hostil no pueda escribir fuera del destino; `--remote-root` confina al agente (traversal y symlinks que escapan). Con `--delete`: `--max-delete`, confirmación desde 10 borrados, lo excluido nunca se borra, y no se borra nada si hubo errores.

## Exclusiones

Patrones al estilo rsync (subconjunto simple, sin `**`; sintaxis de cada patrón: la de `path.Match` — `*`, `?`, `[clase]`):

```sh
vx sync --exclude '*.log' --exclude 'node_modules/' ./proyecto host:/srv/proyecto
vx sync --exclude-from .vxignore ./proyecto host:/srv/proyecto
```

| Forma del patrón | Dónde matchea |
|---|---|
| sin `/` (`*.log`, `node_modules`) | el nombre, a cualquier profundidad |
| con `/` (`cache/tmp`) | se ancla a la ruta relativa completa desde la raíz sincronizada |
| termina en `/` (`.git/`) | sólo directorios (y por lo tanto todo lo que hay adentro) |

Se suman tres fuentes, todas activas a la vez: `exclude:` en `~/.config/vextra/config.yaml` (patrones permanentes,
formato lista: `- "*.tmp"`), `--exclude-from ARCHIVO` (un patrón por línea, `#` comenta) y `--exclude` (repetible).
Lo excluido no se transfiere **ni se borra** con `--delete`, aunque ya no exista en el origen — es la forma de,
por ejemplo, dejar un `.git/` o unos logs locales del lado del destino sin que una sincronización los toque.
Un archivo pasado como origen explícito (`vx get host:/etc/passwd ./x`) no pasa por los filtros: sólo aplican
al recorrer un árbol.

## Rendimiento (fase 3)

Lo que ya construye la fase 3 de la spec: **compresión adaptativa** (arriba) y **concurrencia acotada**
(`workers`, `max_inflight`, `--bwlimit`) vienen desde el primer commit; el bloque de delta adaptativo (arriba)
también reduce trabajo real, no sólo bytes en el cable. Lo que falta y es explícitamente el criterio de salida
de esta fase — **"igual o mejor que rsync en escenarios típicos, medido"** — es la medición en sí:

```sh
make bench                                  # transporte ssh simulado en esta máquina (sin red real)
HOST=usuario@servidor ./scripts/bench.sh    # con una red real: el número que cuenta
```

Corre tres escenarios (copia inicial en frío, re-sync sin cambios, editar ~1% de un archivo grande) con `vx`,
`rsync` y `scp`, y reporta tiempo y bytes en el cable de cada uno (bytes: la propia línea de resumen de `vx`
y "Total bytes sent/received" de `rsync --stats`; `scp` no tiene delta ni estadísticas, así que sólo entra en
la comparación de tiempo). Con `HOST` sin definir usa el mismo mecanismo de `demo.sh` (un "ssh" que ejecuta el
comando en la misma máquina): sirve para comparar el trabajo de cada herramienta, pero no hay ancho de banda ni
latencia real de por medio, así que **los tiempos ahí no son un benchmark real** — hace falta `HOST=usuario@servidor`
contra una red de verdad para que el número tenga sentido.

**Estado honesto:** el script está escrito y probé su lógica de parseo (tiempo, bytes de `--stats`) con datos de
muestra, pero no lo corrí de punta a punta: este entorno no tiene `rsync`, `scp` ni `ssh` instalados, ni red. No
hay ningún número real todavía — hace falta que alguien lo corra. Tampoco se migró a BLAKE3/zstd (siguen
SHA-256/DEFLATE; ver la tabla de abajo), así que el resultado de hoy no refleja el rendimiento final esperado.

## Backups

`vx sync --delete` sirve como el motor de transferencia de un flujo de backup, en el mismo lugar donde hoy se
usa `rsync -a --delete`: espejo exacto, delta para no re-mandar lo que no cambió, reanudación si se corta, y las
salvaguardas de borrado (`--max-delete`, confirmación) para no vaciar un backup por accidente. Combinado con
`--exclude` (para dejar afuera `.git/`, cachés, `node_modules`) y cron o systemd timers, cubre el caso de uso
típico de "espejar este directorio a otra máquina todas las noches":

```sh
vx sync --delete --max-delete 500 --exclude '.git/' --exclude '*.tmp' \
  /datos usuario@backup-host:/srv/backups/datos
```

Lo que **no** es: un repositorio de backup con deduplicación, snapshots versionados y retención (tipo
Borg/restic). Eso está explícitamente en el backlog de la spec (§18) — mezclarlo duplicaría el alcance de V1.
Si necesitás versionar cada corrida (un directorio por día, con hardlinks a lo que no cambió, al estilo
`rsync --link-dest`), hoy hay que armarlo por fuera (por ejemplo, `cp -al` del último backup antes de correr
`vx sync --delete` encima, o un destino distinto por fecha); ese patrón queda anotado como una posible mejora
puntual, no como el rediseño de repositorio que la spec deja para más adelante.

## Cobertura de la spec y desvíos deliberados

Cubierto: §3 completo salvo lo indicado abajo, §5 (shell y modo comando), §6, §7, §9, §11.2–11.4, §13, fases 0–2
completas y buena parte de 3 (falta medir; ver "Rendimiento" arriba) y 4.

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
listados se arman en memoria (~100 B/entrada) · el tamaño de bloque adaptativo depende sólo del tamaño del
archivo, no de dónde cambió (en archivos grandes una sola edición chica igual puede caer en un bloque de varios
cientos de KiB) · no se preservan owner/xattrs/hardlinks (sí permisos y mtime, de archivos y de directorios) · sockets y
dispositivos se ignoran · un directorio ilegible aborta la corrida (a propósito: con `--delete` no puede
confundirse con "vacío") · las exclusiones no soportan `**` ni reglas de inclusión que reviertan una exclusión
más general (como el `!patrón` de `.gitignore`) · el shell no tiene edición de línea ni historial (usar
`rlwrap vx connect ...`) · nombres de archivo de ~230+ bytes no admiten el sufijo del temporal.

## Próximos pasos sugeridos

1. Compilar y pasar `make test`. 2. `make bench` con `HOST=` contra un servidor real — el primer número real de
la fase 3. 3. Según lo que muestre eso: BLAKE3 + zstd (probablemente lo que más impacto tenga en CPU/bytes).
4. `--json` para automatización. 5. Transporte `x/crypto/ssh` nativo y/o fallback SFTP. 6. Si hace falta backup
versionado de verdad: repositorio con dedup/snapshots/retención (backlog explícito de la spec, fase v4).