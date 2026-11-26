#!/usr/bin/env bash
# Model-checks every specification in this directory with TLC.
#
#   tla/check.sh            every configuration
#   tla/check.sh Breaker    those of one module
#
# <Module>.cfg must hold. <Module>.fails.cfg is a configuration of something
# known to be wrong, which TLC must find violated: it shows the property
# beside it is one the model can break.
#
# TLC is tla2tools.jar of the release pinned here, fetched once into the
# cache directory and held to its checksum; TLA2TOOLS_JAR names another copy.
# It needs Java 11 or later (mise.toml).
set -euo pipefail
cd "$(dirname "$0")"

version=1.7.4
sha256=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88

jar=${TLA2TOOLS_JAR:-}
if [[ -z $jar ]]; then
    cache=${XDG_CACHE_HOME:-$HOME/.cache}/s3rp
    jar=$cache/tla2tools-$version.jar
    if [[ ! -f $jar ]]; then
        mkdir -p "$cache"
        curl -fsSL -o "$jar.tmp" "https://github.com/tlaplus/tlaplus/releases/download/v$version/tla2tools.jar"
        mv "$jar.tmp" "$jar"
    fi
    echo "$sha256  $jar" | sha256sum --check --quiet
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

failed=0
for cfg in "${1:-}"*.cfg; do
    module=${cfg%%.*}
    name=${cfg%.cfg}
    log=$work/$name.log
    code=0
    # -deadlock: a request that ended and a stream that was read have no
    # next step, which is not an error
    java -XX:+UseParallelGC -cp "$jar" tlc2.TLC -deadlock -workers auto \
        -metadir "$work/$name" -config "$cfg" "$module.tla" >"$log" 2>&1 || code=$?
    states=$(sed -n 's/^\([0-9]*\) states generated, \([0-9]*\) distinct states found.*/\2 states/p' "$log" | tail -1)
    if [[ $cfg == *.fails.cfg ]]; then
        # 12 and 13 are TLC's exit codes for a violated invariant and a
        # violated temporal property
        if [[ $code -eq 12 || $code -eq 13 ]] && grep -q 'is violated' "$log"; then
            echo "ok    $name: $(grep -m1 'is violated' "$log" | sed 's/^Error: //')"
            continue
        fi
        echo "FAIL  $name: expected a violation, TLC exited $code"
    elif [[ $code -eq 0 ]] && grep -q 'No error has been found' "$log"; then
        echo "ok    $name: $states"
        continue
    else
        echo "FAIL  $name: TLC exited $code"
    fi
    failed=1
    cat "$log"
done
exit $failed
