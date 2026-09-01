#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
check_dir="$repo_dir/.cache/opencypher/M23/generated-check"
mkdir -p "$check_dir"
OUTPUT_DIR="$check_dir" "$repo_dir/tools/cypher/generate.sh"
for file in cypher_lexer.go cypher_parser.go cypher_visitor.go cypher_base_visitor.go; do
	diff -u "$repo_dir/internal/cypher/parsergen/$file" "$check_dir/$file"
done
