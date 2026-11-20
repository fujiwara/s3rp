#!/usr/bin/env bash
# winscp.sh VERB ARG...: the verbs through WinSCP's scripting, run under
# Wine. Each call is one session: a script that opens it, runs the
# commands given as arguments, and exits.
set -euo pipefail

# Wine maps Z: to the root directory
win() { printf 'Z:%s' "${1//\//\\}"; }
# a script argument is double-quoted, with a quote inside doubled
q() { printf '"%s"' "${1//\"/\"\"}"; }

session() {
    local script=$STATE_DIR/session.txt options="S3UrlStyle=1" certificate=""
    if [ "$SCHEME" = https ]; then
        certificate='-certificate="*"'
    else
        options="$options Ftps=0"
    fi
    {
        echo "option batch abort"
        echo "option confirm off"
        echo "open s3://127.0.0.1:$PORT/ -username=$ACCESS_KEY -password=$SECRET_KEY $certificate -rawsettings $options"
        printf '%s\n' "$@"
        echo "exit"
    } > "$script"
    local run=(wine "$TOOLS/winscp/WinSCP.com" /ini=nul "/log=$(win "$STATE_DIR/winscp.log")" "/script=$(win "$script")")
    # WinSCP.com starts WinSCP.exe, a GUI program that needs a display even
    # when it never shows a window
    [ -n "${DISPLAY:-}" ] || run=(xvfb-run -a "${run[@]}")
    "${run[@]}" < /dev/null | tr -d '\r'
}

verb=$1
shift
case $verb in
    put)      session "put -nopermissions -nopreservetime $(q "$(win "$1")") $(q "/$BUCKET/$2")" ;;
    get)      session "get -nopreservetime $(q "/$BUCKET/$1") $(q "$(win "$2")")" ;;
    # a file's line starts with its permissions; the rest is the session's chatter
    list)     session "ls $(q "/$BUCKET/$1/")" | grep '^-' ;;
    delete)   session "rm $(q "/$BUCKET/$1")" ;;
    copy)     session "cp $(q "/$BUCKET/$1") $(q "/$BUCKET/$2")" ;;
    put_tree) session "put -nopermissions -nopreservetime $(q "$(win "$1")\\*") $(q "/$BUCKET/$2/")" ;;
    *)        echo "unknown verb: $verb" >&2; exit 2 ;;
esac
