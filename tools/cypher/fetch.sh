#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cache_dir=${CYPHER_CACHE_DIR:-"$repo_dir/.cache/opencypher/M23"}
antlr_cache=${ANTLR_CACHE_DIR:-"$repo_dir/.cache/antlr/4.13.1"}

grammar_url=https://s3.amazonaws.com/artifacts.opencypher.org/M23/Cypher.g4
tck_url=https://s3.amazonaws.com/artifacts.opencypher.org/M23/tck-M23.zip
antlr_url=https://repo1.maven.org/maven2/org/antlr/antlr4/4.13.1/antlr4-4.13.1-complete.jar
upstream_commit=007895aff5f33097d67b2e48a0a2babd6bd18590
binary_tree_1_url=https://raw.githubusercontent.com/opencypher/openCypher/$upstream_commit/tck/graphs/binary-tree-1/binary-tree-1.cypher
binary_tree_2_url=https://raw.githubusercontent.com/opencypher/openCypher/$upstream_commit/tck/graphs/binary-tree-2/binary-tree-2.cypher

grammar_sha=044d58feaccb263f2ec75f181f0f3153e8715b5013fc691d21da22592a58d62a
tck_sha=6deb4acffb301c926cb0811e11b2422704cad2e48fc0a42e40c401a7ee1fba49
antlr_sha=bc13a9c57a8dd7d5196888211e5ede657cb64a3ce968608697e4f668251a8487
binary_tree_1_sha=fbcada6966edb9e2d66b1a11a4f8a4c906a9da6afd622640ab08c686962d42da
binary_tree_2_sha=923bcaf5686ea9051f46ad5c440a286381fd111daad5dbd6377aa3edd7dbfc4c

mkdir -p "$cache_dir/graphs" "$antlr_cache"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

fetch_verified() {
	url=$1
	destination=$2
	expected=$3
	if [ -f "$destination" ] && [ "$(sha256 "$destination")" = "$expected" ]; then
		return
	fi
	temporary="$destination.download"
	curl --fail --location --silent --show-error "$url" --output "$temporary"
	actual=$(sha256 "$temporary")
	if [ "$actual" != "$expected" ]; then
		echo "checksum mismatch for $url: expected $expected, got $actual" >&2
		exit 1
	fi
	mv "$temporary" "$destination"
}

fetch_verified "$grammar_url" "$cache_dir/Cypher.g4" "$grammar_sha"
fetch_verified "$tck_url" "$cache_dir/tck-M23.zip" "$tck_sha"
fetch_verified "$antlr_url" "$antlr_cache/antlr4-4.13.1-complete.jar" "$antlr_sha"
fetch_verified "$binary_tree_1_url" "$cache_dir/graphs/binary-tree-1.cypher" "$binary_tree_1_sha"
fetch_verified "$binary_tree_2_url" "$cache_dir/graphs/binary-tree-2.cypher" "$binary_tree_2_sha"
