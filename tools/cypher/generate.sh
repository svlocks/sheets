#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cache_dir=${CYPHER_CACHE_DIR:-"$repo_dir/.cache/opencypher/M23"}
antlr_cache=${ANTLR_CACHE_DIR:-"$repo_dir/.cache/antlr/4.13.1"}
output_dir=${OUTPUT_DIR:-"$repo_dir/internal/cypher/parsergen"}
derived_dir="$cache_dir/derived"
generated_dir="$cache_dir/generated"
derived_sha=110c3dc3b70718166caf1042f92844527e889e9c80de92fa9fc3d79c2e74a1cf
image=eclipse-temurin:17.0.16_8-jre-noble@sha256:88665729998a41823f35092c95d574581719371a0f21f62d31a9ccf506b199c6

"$repo_dir/tools/cypher/fetch.sh"
mkdir -p "$derived_dir" "$generated_dir" "$output_dir"
cp "$cache_dir/Cypher.g4" "$derived_dir/Cypher.g4"
patch --batch --forward --silent -d "$derived_dir" -p1 < "$repo_dir/tools/cypher/Cypher.extensions.patch"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$derived_dir/Cypher.g4" | awk '{print $1}')
else
	actual=$(shasum -a 256 "$derived_dir/Cypher.g4" | awk '{print $1}')
fi
if [ "$actual" != "$derived_sha" ]; then
	echo "derived grammar checksum mismatch: expected $derived_sha, got $actual" >&2
	exit 1
fi

docker run --rm --user "$(id -u):$(id -g)" \
	-v "$derived_dir:/grammar:ro" \
	-v "$antlr_cache:/tool:ro" \
	-v "$generated_dir:/out" \
	-w /grammar \
	"$image" \
	java -jar /tool/antlr4-4.13.1-complete.jar \
	-Dlanguage=Go -package parsergen -visitor -no-listener -Xexact-output-dir \
	-o /out Cypher.g4

cp "$generated_dir/cypher_lexer.go" "$output_dir/cypher_lexer.go"
cp "$generated_dir/cypher_parser.go" "$output_dir/cypher_parser.go"
cp "$generated_dir/cypher_visitor.go" "$output_dir/cypher_visitor.go"
cp "$generated_dir/cypher_base_visitor.go" "$output_dir/cypher_base_visitor.go"

gofmt -w "$output_dir"/*.go
