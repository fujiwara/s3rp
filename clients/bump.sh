#!/usr/bin/env bash
# Bumps every pin of the client compatibility run to what clients/README.md
# ("Versions") says a pin is bumped to. It edits the files and stops there:
# a run (./clients/run.sh) says whether the bump holds.
#
#   ./clients/bump.sh
set -euo pipefail

cd "$(dirname "$0")"
mise trust --quiet 2>/dev/null || true
mise --cd .. trust --quiet 2>/dev/null || true
mise --cd .. install --quiet
mise install --quiet
eval "$(mise env --shell bash)"

# replace OLD with NEW on the lines of FILE matching the regexp LINE
replace() { # FILE LINE OLD NEW
    [ "$3" = "$4" ] && return
    LINE=$2 OLD=$3 NEW=$4 perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/ if /$ENV{LINE}/' "$1"
    grep -E -- "$2" "$1" | grep -qF -- "$4" || { echo "$1: could not replace $3 with $4" >&2; exit 1; }
    echo "  $(basename "$1"): $3 -> $4"
}

# the later of two versions
newer() { printf '%s\n' "$1" "$2" | sort -V | tail -n 1; }

echo "==> mise.toml"
# mise leaves out a release younger than minimum_release_age (mise.toml)
for tool in $(yq -p toml -o json '.tools | keys' mise.toml | jq -r '.[]'); do
    current=$(yq -p toml -o json '.tools' mise.toml | jq -r --arg t "$tool" '.[$t] | .version? // .')
    case $tool in
    # the newest LTS line
    java) latest=$(mise ls-remote java 2>/dev/null | grep -E '^temurin-[0-9].*\.LTS$' | tail -n 1) ;;
    # the newest LTS line (.NET) or even-numbered line (Node.js): both even
    dotnet | node) latest=$(mise ls-remote "$tool" 2>/dev/null | grep -E '^[0-9]*[02468]\.[0-9.]+$' | tail -n 1) ;;
    *) latest=$(mise latest "$tool" 2>/dev/null) ;;
    esac
    [ -n "$latest" ] || { echo "no release found for $tool" >&2; exit 1; }
    # never back to a release older than the pin
    replace mise.toml "^\"?$(sed 's/[.+]/\\&/g' <<<"$tool")\"? = " "$current" "$(newer "$current" "$latest")"
done
mise install --quiet
eval "$(mise env --shell bash)"

java_line=$(yq -p toml -o json '.tools.java' mise.toml | jq -r . | sed -E 's/^temurin-([0-9]+).*/\1/')
replace lib/sdk/java/Client.java '^//JAVA ' "$(sed -n 's|^//JAVA ||p' lib/sdk/java/Client.java)" "$java_line"

python_line=$(mise ls-remote python 2>/dev/null | grep -E '^3\.[0-9]+\.[0-9]+$' | tail -n 1 | cut -d. -f1,2)
for f in lib/sdk/python.py lib/obstore_client.py lib/opendal_client.py; do
    replace "$f" '^# requires-python' "$(sed -n 's/^# requires-python = "==\(.*\)\.\*"/\1/p' "$f")" "$python_line"
done

echo "==> Python"
pypi() { curl -fsS "https://pypi.org/pypi/$1/json" | jq -r .info.version; }
bump_pypi() { # FILE PACKAGE
    replace "$1" '^# dependencies' "$2==$(sed -n "s/.*\"$2==\([^\"]*\)\".*/\1/p" "$1")" "$2==$(pypi "$2")"
}
bump_pypi lib/sdk/python.py boto3
bump_pypi lib/obstore_client.py obstore
bump_pypi lib/opendal_client.py opendal

echo "==> JavaScript"
(
    cd lib/sdk/js
    before=$(jq -c .dependencies package.json)
    # the SDK is not held back, whatever the user's npm configuration says
    # shellcheck disable=SC2046
    npm install --silent --package-lock-only --save-exact --min-release-age=0 $(jq -r '.dependencies | keys[] | . + "@latest"' package.json)
    [ "$before" = "$(jq -c .dependencies package.json)" ] || echo "  package.json: $(jq -c .dependencies package.json)"
)

echo "==> Go"
(
    cd lib/sdk/go
    go get -u ./... 2>&1 | sed -n 's/^go: upgraded /  /p'
    go mod tidy
)

echo "==> Java"
maven() { curl -fsS "https://repo1.maven.org/maven2/$1/maven-metadata.xml" | yq -p xml '.metadata.versioning.release'; }
sdk_java=$(maven software/amazon/awssdk/s3)
for artifact in s3 s3-transfer-manager; do
    replace lib/sdk/java/Client.java "^//DEPS software.amazon.awssdk:$artifact:" \
        "$(sed -n "s|^//DEPS software.amazon.awssdk:$artifact:||p" lib/sdk/java/Client.java)" "$sdk_java"
done

echo "==> .NET"
nuget=$(curl -fsS https://api.nuget.org/v3-flatcontainer/awssdk.s3/index.json | jq -r '[.versions[] | select(test("-") | not)] | last')
replace lib/sdk/dotnet/client.cs '^#:package AWSSDK.S3@' "$(sed -n 's/^#:package AWSSDK.S3@//p' lib/sdk/dotnet/client.cs)" "$nuget"

echo "==> Ruby"
for gem in aws-sdk-s3 rexml; do
    replace lib/sdk/ruby.rb "^  gem \"$gem\"" "$(sed -n "s/^  gem \"$gem\", \"\(.*\)\"/\1/p" lib/sdk/ruby.rb)" \
        "$(curl -fsS "https://rubygems.org/api/v1/versions/$gem/latest.json" | jq -r .version)"
done

echo "==> PHP"
# the composer image brings PHP
image=$(sed -n 's/^IMAGE=composer://p' lib/sdk/php.sh)
replace lib/sdk/php.sh '^IMAGE=' "$image" "$(curl -fsS 'https://hub.docker.com/v2/repositories/library/composer/tags?page_size=100' |
    jq -r '.results[].name | select(test("^[0-9]+\\.[0-9]+\\.[0-9]+$"))' | sort -V | tail -n 1)"
replace lib/sdk/php/composer.json 'aws/aws-sdk-php' "$(jq -r '.require["aws/aws-sdk-php"]' lib/sdk/php/composer.json)" \
    "$(curl -fsS https://repo.packagist.org/p2/aws/aws-sdk-php.json | jq -r '.packages["aws/aws-sdk-php"][0].version')"
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e COMPOSER_HOME=/tmp/composer -v "$PWD/lib/sdk/php:/app" \
    "composer:$(sed -n 's/^IMAGE=composer://p' lib/sdk/php.sh)" \
    composer update --working-dir /app --no-install --no-interaction --no-progress --quiet

echo "==> Rust"
for crate in $(yq -p toml -o json '.dependencies | keys' lib/sdk/rust/Cargo.toml | jq -r '.[]'); do
    current=$(yq -p toml -o json '.dependencies' lib/sdk/rust/Cargo.toml | jq -r --arg c "$crate" '.[$c] | .version? // .')
    latest=$(curl -fsS -A s3rp-clients-bump "https://crates.io/api/v1/crates/$crate" | jq -r .crate.max_stable_version)
    replace lib/sdk/rust/Cargo.toml "^$crate = " "$current" "=$latest"
done
cargo update --quiet --manifest-path lib/sdk/rust/Cargo.toml

# The two downloads: the version is in the URL and in the name the file is
# kept under, and the SHA-256 beside it.
echo "==> duck"
stanza=$(curl -fsS https://s3.amazonaws.com/repo.deb.cyberduck.io/dists/stable/main/binary-amd64/Packages |
    awk -v RS= '/(^|\n)Package: duck\n/' | awk -v RS= 'END {print}')
old_file=$(grep -o 'duck_[0-9.]*_amd64\.deb' defs/duck.yml)
new_file=$(sed -n 's|^Filename: .*/||p' <<<"$stanza")
old_version=$(sed -E 's/^duck_([0-9]+\.[0-9]+\.[0-9]+)\..*/\1/' <<<"$old_file")
new_version=$(sed -E 's/^duck_([0-9]+\.[0-9]+\.[0-9]+)\..*/\1/' <<<"$new_file")
if [ "$old_file" != "$new_file" ]; then
    replace defs/duck.yml '^ +[0-9a-f]{64} ' "$(grep -oE '^ +[0-9a-f]{64} ' defs/duck.yml | tr -d ' ')" "$(sed -n 's/^SHA256: //p' <<<"$stanza")"
    replace defs/duck.yml 'duck_' "$old_file" "$new_file"
    replace defs/duck.yml 'duck_' "duck_$old_version.deb" "duck_$new_version.deb"
fi

echo "==> winscp"
old_version=$(grep -oE 'WinSCP/[0-9.]+/' defs/winscp.yml | cut -d/ -f2)
new_version=$(curl -fsSL https://sourceforge.net/projects/winscp/best_release.json | jq -r .release.filename | cut -d/ -f3)
if [ "$old_version" != "$new_version" ]; then
    # SourceForge publishes no SHA-256: this pins what it serves today
    sum=$(curl -fsSL "https://sourceforge.net/projects/winscp/files/WinSCP/$new_version/WinSCP-$new_version-Portable.zip/download" | sha256sum | cut -d' ' -f1)
    replace defs/winscp.yml '^ +[0-9a-f]{64} ' "$(grep -oE '^ +[0-9a-f]{64} ' defs/winscp.yml | tr -d ' ')" "$sum"
    replace defs/winscp.yml 'WinSCP' "$old_version" "$new_version"
    replace defs/winscp.yml 'WinSCP' "$old_version" "$new_version"
fi
