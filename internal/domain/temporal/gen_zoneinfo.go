//go:build ignore

// gen_zoneinfo compiles Sheets's authenticated IANA timezone profile in a
// digest-pinned container. It is intentionally excluded from normal builds.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	tzdbVersion           = "2023c"
	tzcodeSHA256          = "46d17f2bb19ad73290f03a203006152e0fa0d7b11e5b71467c4a823811b214e7"
	tzdataSHA256          = "3f510b5d1b4ae9bb38e485aa302a776b317fb3637bdb6404c4adf7b6cadd965c"
	expectedArchiveSHA256 = "3fe2fe0c5897093e4965480de18722eabc224a1b7ac4dcb1ceb6943d62c01efe"

	// The digest is the multi-platform manifest for the official Go image. The
	// resulting archive is byte-identical on its linux/amd64 and linux/arm64
	// variants. The image provides Go's deterministic mkzip program plus the C
	// compiler, make, awk, curl, and tar used below.
	buildImage = "docker.io/library/golang:1.25.14-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437"
)

var (
	checkOnly     = flag.Bool("check", false, "verify that zoneinfo.zip is reproducible without replacing it")
	buildPlatform = flag.String("platform", "", "optional container platform, for example linux/amd64")
)

func main() {
	flag.Parse()
	if flag.NArg() != 0 {
		fatalf("unexpected arguments: %v", flag.Args())
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		fatalf("locate generator source directory")
	}
	packageDirectory := filepath.Dir(sourceFile)
	buildDirectory, err := os.MkdirTemp(packageDirectory, ".zoneinfo-build-")
	if err != nil {
		fatalf("create build directory: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(buildDirectory); err != nil {
			fmt.Fprintf(os.Stderr, "gen_zoneinfo: remove build directory: %v\n", err)
		}
	}()

	runtime := os.Getenv("SHEETS_CONTAINER_RUNTIME")
	if runtime == "" {
		runtime = "docker"
	}
	outputPath := filepath.Join(buildDirectory, "zoneinfo.zip")
	buildScript := fmt.Sprintf(containerBuildScript, tzdbVersion, tzcodeSHA256, tzdataSHA256)
	arguments := []string{
		"run", "--rm", "--pull=missing",
		"--mount", "type=bind,source=" + buildDirectory + ",target=/out",
	}
	if *buildPlatform != "" {
		arguments = append(arguments, "--platform", *buildPlatform)
	}
	arguments = append(arguments, buildImage, "sh", "-ceu", buildScript)
	command := exec.Command(runtime, arguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		fatalf("run pinned timezone build: %v", err)
	}

	generated, err := os.ReadFile(outputPath)
	if err != nil {
		fatalf("read generated archive: %v", err)
	}
	if actual := checksum(generated); actual != expectedArchiveSHA256 {
		fatalf("generated zoneinfo.zip SHA-256 %s, want %s", actual, expectedArchiveSHA256)
	}

	destination := filepath.Join(packageDirectory, "zoneinfo.zip")
	if *checkOnly {
		committed, err := os.ReadFile(destination)
		if err != nil {
			fatalf("read committed archive: %v", err)
		}
		if !bytes.Equal(generated, committed) {
			fatalf("%s is stale (generated SHA-256 %s, committed SHA-256 %s)",
				destination, checksum(generated), checksum(committed))
		}
		fmt.Printf("%s %s (%s, IANA %s main profile) is reproducible\n",
			destination, expectedArchiveSHA256, buildImage, tzdbVersion)
		return
	}

	temporary, err := os.CreateTemp(".", "zoneinfo.zip.*")
	if err != nil {
		fatalf("create temporary archive: %v", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		fatalf("set archive permissions: %v", err)
	}
	if _, err := temporary.Write(generated); err != nil {
		_ = temporary.Close()
		fatalf("write archive: %v", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		fatalf("sync archive: %v", err)
	}
	if err := temporary.Close(); err != nil {
		fatalf("close archive: %v", err)
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		fatalf("replace archive: %v", err)
	}
	fmt.Printf("generated %s %s from IANA %s main profile\n", destination, expectedArchiveSHA256, tzdbVersion)
}

func checksum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "gen_zoneinfo: "+format+"\n", arguments...)
	os.Exit(1)
}

// IANA tzdb 2023c's default main profile deliberately leaves PACKRATDATA and
// PACKRATLIST empty. This is the post-1970 equivalence policy used by the M23
// temporal fixtures. In particular it keeps all of the aliases moved in IANA
// 2022b as links to their main-profile zones; it does not special-case
// Europe/Stockholm. The downloads are accepted only after their exact SHA-256
// digests match. IANA's LICENSE places the data and zic inputs used here in the
// public domain; its separately identified BSD files are not incorporated.
// The resulting archive likewise does not incorporate the BSD-licensed Go
// mkzip source used to package it.
const containerBuildScript = `
export LC_ALL=C
export TZ=UTC
export SOURCE_DATE_EPOCH=0
umask 022

work=/tmp/sheets-tzdb-build
mkdir -p "$work/zoneinfo"
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --output "$work/tzcode.tar.gz" \
  https://data.iana.org/time-zones/releases/tzcode%[1]s.tar.gz
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --output "$work/tzdata.tar.gz" \
  https://data.iana.org/time-zones/releases/tzdata%[1]s.tar.gz
printf '%%s  %%s\n' \
  '%[2]s' \
  "$work/tzcode.tar.gz" | sha256sum --check --strict
printf '%%s  %%s\n' \
  '%[3]s' \
  "$work/tzdata.tar.gz" | sha256sum --check --strict
tar -xzf "$work/tzcode.tar.gz" -C "$work"
tar -xzf "$work/tzdata.tar.gz" -C "$work"
make --silent -C "$work" \
  CFLAGS=-DSTD_INSPIRED AWK=awk TZDIR="$work/zoneinfo" \
  BACKWARD=backward PACKRATDATA= PACKRATLIST= posix_only
cd "$work/zoneinfo"
GOTOOLCHAIN=local go run /usr/local/go/lib/time/mkzip.go /out/zoneinfo.zip
sha256sum /out/zoneinfo.zip
`
