#!/usr/bin/env bash
# The AWS SDK for PHP of defs/sdk-php.yml, run in the composer image: mise
# has no prebuilt PHP. The run's work directory is mounted at its own path,
# so the files a verb names are the same inside.
#
#   php.sh install        installs the SDK composer.json pins
#   php.sh <verb> ...     runs a verb of client.php
set -euo pipefail
IMAGE=composer:2.11.2
dir=$(dirname "$0")/php
work=$(dirname "$TOOLS")

run() {
    docker run --rm --network host --user "$(id -u):$(id -g)" \
        -e HOME=/tmp -e COMPOSER_HOME=/tmp/composer \
        -e ENDPOINT -e BUCKET -e ACCESS_KEY -e SECRET_KEY -e CA_FILE -e VENDOR="$TOOLS/sdk-php/vendor" \
        -v "$dir:/app:ro" -v "$work:$work" "$IMAGE" "$@"
}

if [ "$1" = install ]; then
    mkdir -p "$TOOLS/sdk-php"
    cp "$dir/composer.json" "$dir/composer.lock" "$TOOLS/sdk-php/"
    run composer install --working-dir "$TOOLS/sdk-php" --no-interaction --no-progress --quiet
else
    run php /app/client.php "$@"
fi
