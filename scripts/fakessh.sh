#!/bin/sh
# Simula `ssh [opciones] HOST COMANDO` ejecutando COMANDO en esta misma máquina.
# Sirve para demos y pruebas sin sshd:
#     VX_SSH="$PWD/scripts/fakessh.sh" vx put ./dir demo:/tmp/destino
# Ignora las opciones de ssh y el nombre de host.
while [ $# -gt 0 ]; do
  case "$1" in
    -o|-p|-i|-l|-F|-J|-b|-c|-E|-e|-m|-O|-Q|-S|-w|-L|-R|-D|-B|-I|-W) shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
shift # descarta el host
exec sh -c "$1"
