package temporal

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	// PinnedTZDBVersion identifies the upstream Go distribution whose complete
	// IANA archive is embedded below. Updating timezone rules is an explicit
	// repository change, never an implicit consequence of the host OS.
	PinnedTZDBVersion = "go1.25.14"
	// PinnedTZDBSHA256 authenticates the exact upstream zoneinfo.zip asset.
	PinnedTZDBSHA256 = "33bd7c3c9bc812f1b4dacf7b9516aa7a129acd658b90f239cb8a286d73cedd0f"

	maximumPinnedZoneBytes = 1 << 20
)

// Regenerate only as part of a deliberate timezone database update. The
// generator refuses any toolchain or archive other than the constants above.
//
//go:generate env GOTOOLCHAIN=go1.25.14 go run gen_zoneinfo.go

//go:embed zoneinfo.zip
var pinnedTZDBArchive []byte

var (
	pinnedTZDBOnce      sync.Once
	pinnedTZDBFiles     map[string]*zip.File
	pinnedTZDBInitError error
	pinnedLocations     sync.Map
)

// PinnedZoneDatabase resolves named zones exclusively from Sheets's embedded
// timezone archive. It never consults ZONEINFO, the host filesystem,
// time.Local, or the running Go installation.
type PinnedZoneDatabase struct{}

// LoadLocation implements ZoneDatabase.
func (PinnedZoneDatabase) LoadLocation(name string) (*time.Location, error) {
	if cached, ok := pinnedLocations.Load(name); ok {
		return cached.(*time.Location), nil
	}
	pinnedTZDBOnce.Do(initializePinnedTZDB)
	if pinnedTZDBInitError != nil {
		return nil, fmt.Errorf("%w: initialize pinned timezone database: %v", ErrInvalid, pinnedTZDBInitError)
	}
	file, ok := pinnedTZDBFiles[name]
	if !ok {
		return nil, fmt.Errorf("%w: load timezone %q from %s: unknown timezone", ErrInvalid, name, PinnedTZDBVersion)
	}
	if file.UncompressedSize64 > maximumPinnedZoneBytes {
		return nil, fmt.Errorf("%w: timezone %q payload is too large", ErrInvalid, name)
	}
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: open timezone %q from %s: %v", ErrInvalid, name, PinnedTZDBVersion, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maximumPinnedZoneBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("%w: read timezone %q from %s: %v", ErrInvalid, name, PinnedTZDBVersion, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("%w: close timezone %q from %s: %v", ErrInvalid, name, PinnedTZDBVersion, closeErr)
	}
	if len(data) > maximumPinnedZoneBytes {
		return nil, fmt.Errorf("%w: timezone %q payload is too large", ErrInvalid, name)
	}
	location, err := time.LoadLocationFromTZData(name, data)
	if err != nil {
		return nil, fmt.Errorf("%w: parse timezone %q from %s: %v", ErrInvalid, name, PinnedTZDBVersion, err)
	}
	actual, _ := pinnedLocations.LoadOrStore(name, location)
	return actual.(*time.Location), nil
}

func initializePinnedTZDB() {
	digest := sha256.Sum256(pinnedTZDBArchive)
	if hex.EncodeToString(digest[:]) != PinnedTZDBSHA256 {
		pinnedTZDBInitError = fmt.Errorf("archive checksum does not match %s", PinnedTZDBSHA256)
		return
	}
	archive, err := zip.NewReader(bytes.NewReader(pinnedTZDBArchive), int64(len(pinnedTZDBArchive)))
	if err != nil {
		pinnedTZDBInitError = err
		return
	}
	files := make(map[string]*zip.File, len(archive.File))
	for _, file := range archive.File {
		if _, duplicate := files[file.Name]; duplicate {
			pinnedTZDBInitError = fmt.Errorf("duplicate timezone %q", file.Name)
			return
		}
		files[file.Name] = file
	}
	pinnedTZDBFiles = files
}
