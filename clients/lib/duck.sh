#!/usr/bin/env bash
# duck.sh VERB ARG...: the verbs through Cyberduck's command line.
set -euo pipefail
duck() {
    "$TOOLS/duck/opt/duck/bin/duck" --quiet --assumeyes --nokeychain \
        --username "$ACCESS_KEY" --password "$SECRET_KEY" "$@" < /dev/null
}
# the scheme is the connection profile's (see the definition's setup)
url=s3rp://127.0.0.1:$PORT/$BUCKET

# Cyberduck uploads only into a folder that exists, and makes one a level
# at a time, as a zero-byte object named with a trailing slash.
make_parents() {
    local dir=${1%/*} path="" part
    [ "$dir" != "$1" ] || return 0
    local IFS=/
    for part in $dir; do
        path=$path$part/
        duck --mkdir "$url/$path"
    done
}

verb=$1
shift
case $verb in
    put)      make_parents "$2"; duck --existing overwrite --upload "$url/$2" "$1" ;;
    get)      duck --existing overwrite --download "$url/$1" "$2" ;;
    # over http duck prints a warning about the secret on stdout
    list)     duck --list "$url/$1/" | grep -v 'sent in plaintext' ;;
    delete)   duck --delete "$url/$1" ;;
    put_tree) duck --existing overwrite --upload "$url/$2/" "$1/" ;;
    *)        echo "unknown verb: $verb" >&2; exit 2 ;;
esac
