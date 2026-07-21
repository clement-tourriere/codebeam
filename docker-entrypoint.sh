#!/bin/sh
set -eu

prepare_directory() {
    directory="$1"
    mkdir -p "$directory"

    # PaaS volumes such as Railway's are mounted as root, hiding the ownership
    # prepared in the image. Repair a newly attached (or migrated) volume once;
    # subsequent starts see UID 1000 and skip the recursive walk.
    if [ "$(stat -c '%u' "$directory")" != "1000" ]; then
        chown -R codebeam:codebeam "$directory"
    fi
}

if [ "$(id -u)" = "0" ]; then
    prepare_directory "${CODEBEAM_DATA_DIR:-/data}"
    prepare_directory "${XDG_CONFIG_HOME:-/config}"
    exec su-exec codebeam:codebeam codebeam "$@"
fi

exec codebeam "$@"
