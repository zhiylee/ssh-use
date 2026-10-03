#!/bin/sh
# Bootstrap local bind mounts and credentials; never start the service implicitly.
set -eu

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
    printf 'Usage: %s <server-IP-or-hostname>\n' "$0" >&2
    exit 2
fi
case "$1" in
    *[!a-zA-Z0-9.:-]* | -*)
        printf 'Use a hostname or IP without a scheme, brackets, port, or path.\n' >&2
        exit 2
        ;;
esac
case "$1" in
    *:*:*) ;;
    *:*)
        printf 'Pass the server host without a port; set SSH_USE_PORT in .env.\n' >&2
        exit 2
        ;;
esac

task_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$task_root"
if ! docker compose version >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    printf 'Docker Engine and the Docker Compose plugin (v2 or later) must be available and running.\n' >&2
    exit 1
fi
if [ -e deploy/state/server ]; then
    printf 'deploy/state/server already exists; initialization will not replace credentials.\n' >&2
    exit 1
fi

umask 077
if [ ! -e .env ]; then
    task_uid=$(id -u)
    task_gid=$(id -g)
    if [ "$task_uid" -eq 0 ]; then
        task_uid=10001
        task_gid=10001
    fi
    sed -e "s/^SSH_USE_UID=.*/SSH_USE_UID=$task_uid/" \
        -e "s/^SSH_USE_GID=.*/SSH_USE_GID=$task_gid/" .env.example > .env
fi

# Compose resolves .env and environment overrides without executing shell code.
task_environment=$(docker compose config --environment)
task_uid=$(printf '%s\n' "$task_environment" | sed -n 's/^SSH_USE_UID=//p')
task_gid=$(printf '%s\n' "$task_environment" | sed -n 's/^SSH_USE_GID=//p')
task_port=$(printf '%s\n' "$task_environment" | sed -n 's/^SSH_USE_PORT=//p')
task_uid=${task_uid:-10001}
task_gid=${task_gid:-10001}
task_port=${task_port:-7443}
for task_value in "$task_uid" "$task_gid" "$task_port"; do
    case "$task_value" in
        '' | *[!0-9]*)
            printf 'SSH_USE_UID, SSH_USE_GID and SSH_USE_PORT must be numeric.\n' >&2
            exit 2
            ;;
    esac
done
if [ "$task_uid" -eq 0 ] || [ "$task_port" -lt 1 ] || [ "$task_port" -gt 65535 ]; then
    printf 'Use a non-root SSH_USE_UID and a port between 1 and 65535.\n' >&2
    exit 2
fi
if [ "$(id -u)" -ne 0 ] && { [ "$task_uid" != "$(id -u)" ] || [ "$task_gid" != "$(id -g)" ]; }; then
    printf 'Set SSH_USE_UID/GID to your own IDs in .env, or run initialization as root.\n' >&2
    exit 1
fi

mkdir -p deploy/state/config deploy/state/data deploy/state/ssh
if [ ! -e deploy/state/config/config.yaml ]; then
    cp deploy/config.example.yaml deploy/state/config/config.yaml
fi
if [ ! -e deploy/state/ssh/known_hosts ]; then
    touch deploy/state/ssh/known_hosts
fi
chmod 700 deploy/state deploy/state/config deploy/state/data deploy/state/ssh
chmod 600 deploy/state/config/config.yaml deploy/state/ssh/known_hosts
if [ "$(id -u)" -eq 0 ]; then
    chown "$task_uid:$task_gid" deploy/state deploy/state/config deploy/state/data \
        deploy/state/ssh deploy/state/config/config.yaml deploy/state/ssh/known_hosts
fi

docker compose build ssh-use
docker compose run --rm --no-deps admin server init --dir /bootstrap/server --host "$1" --port "$task_port"
printf '\nInitialized deploy/state. Install the SSH key and verified known_hosts,\n'
printf 'set SSH_USE_BIND_ADDR in .env for remote access, then run docker compose up -d.\n'
