#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/sheets-tck-report.XXXXXX")
first="$temporary_dir/first.json"
second="$temporary_dir/second.json"
cleanup() {
	rm -f "$first" "$second"
	rmdir "$temporary_dir"
}
trap cleanup EXIT HUP INT TERM

"$repo_dir/tools/cypher/fetch.sh"
cd "$repo_dir"
go run ./tools/cypher/tckreport \
	-archive .cache/opencypher/M23/tck-M23.zip \
	-fixtures .cache/opencypher/M23/graphs \
	-manifest tools/cypher/capabilities.json > "$first"
go run ./tools/cypher/tckreport \
	-archive .cache/opencypher/M23/tck-M23.zip \
	-fixtures .cache/opencypher/M23/graphs \
	-manifest tools/cypher/capabilities.json > "$second"
cmp "$first" "$second"
