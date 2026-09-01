//go:build ignore

// gen_zoneinfo copies the authenticated zoneinfo archive from the pinned Go
// toolchain into this package. It is intentionally excluded from normal builds.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	expectedGoVersion = "go1.25.14"
	expectedSHA256    = "33bd7c3c9bc812f1b4dacf7b9516aa7a129acd658b90f239cb8a286d73cedd0f"
)

func main() {
	if runtime.Version() != expectedGoVersion {
		panic(fmt.Sprintf("zoneinfo generation requires %s, got %s", expectedGoVersion, runtime.Version()))
	}
	source := filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip")
	data, err := os.ReadFile(source)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(data)
	if actual := hex.EncodeToString(digest[:]); actual != expectedSHA256 {
		panic(fmt.Sprintf("unexpected %s zoneinfo.zip SHA-256 %s", expectedGoVersion, actual))
	}
	temporary, err := os.CreateTemp(".", "zoneinfo.zip.*")
	if err != nil {
		panic(err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		panic(err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		panic(err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		panic(err)
	}
	if err := temporary.Close(); err != nil {
		panic(err)
	}
	if err := os.Rename(temporaryName, "zoneinfo.zip"); err != nil {
		panic(err)
	}
}
