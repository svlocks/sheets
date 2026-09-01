// Package project contains sheets project initialization and discovery.
package project

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/svlocks/sheets/internal/store"
)

// Names used by a sheets project on disk.
const (
	MetadataDirName  = ".sheets"
	DatabaseFileName = "sheets.db"
	MarkerFileName   = "project"
)

const markerContents = "sheets project v1\n"

var (
	// ErrProjectNotFound is returned when no project can be found above a path.
	ErrProjectNotFound = errors.New("sheets project not found")
	// ErrInvalidLayout means a sheets metadata directory exists but is not a
	// recognizable sheets project.
	ErrInvalidLayout = errors.New("invalid sheets project layout")
	// ErrUnsafeLayout means a project path contains a symlink or other object
	// that could redirect writes outside the project.
	ErrUnsafeLayout = errors.New("unsafe sheets project layout")
)

// LayoutError describes an invalid or unsafe metadata layout. Callers can use
// errors.Is to distinguish invalid and unsafe layouts from ordinary I/O errors.
type LayoutError struct {
	Root   string
	Path   string
	Detail string
	Err    error
}

func (e *LayoutError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("sheets project %s: %s", e.Root, e.Err)
	}
	return fmt.Sprintf("sheets project %s: %s: %s", e.Root, e.Detail, e.Err)
}

func (e *LayoutError) Unwrap() error { return e.Err }

// Project identifies the canonical paths belonging to one sheets project.
// Root is always an absolute, symlink-resolved directory.
type Project struct {
	Root        string
	MetadataDir string
	DBPath      string
}

func projectAt(root string) Project {
	metadata := filepath.Join(root, MetadataDirName)
	return Project{
		Root:        root,
		MetadataDir: metadata,
		DBPath:      filepath.Join(metadata, DatabaseFileName),
	}
}

// Init atomically creates a durable marker and initialized SQLite database. A
// staging directory keeps concurrent discovery from observing partial state.
func Init(path string) (Project, error) {
	return InitContext(context.Background(), path)
}

// InitContext is Init with cancellation for database initialization.
func InitContext(ctx context.Context, path string) (Project, error) {
	if ctx == nil {
		return Project{}, fmt.Errorf("sheets init %q: nil context", path)
	}
	root, err := canonicalInitDirectory(path)
	if err != nil {
		return Project{}, fmt.Errorf("sheets init %q: %w", path, err)
	}
	p := projectAt(root)
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return Project{}, fmt.Errorf("sheets init %q: secure root: %w", path, err)
	}
	defer func() { _ = rootHandle.Close() }()

	info, err := rootHandle.Lstat(MetadataDirName)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return Project{}, layoutError(p, p.MetadataDir, true, "metadata directory is a symlink")
		}
		if !info.IsDir() {
			return Project{}, layoutError(p, p.MetadataDir, true, "metadata path is not a directory")
		}
		return completeExistingProject(ctx, p)
	}
	if !os.IsNotExist(err) {
		return Project{}, fmt.Errorf("sheets init %q: inspect metadata: %w", path, err)
	}

	stageName, stageRoot, err := createStage(rootHandle)
	if err != nil {
		return Project{}, fmt.Errorf("sheets init %q: create staging directory: %w", path, err)
	}
	stageOpen := true
	stagePublished := false
	defer func() {
		if stageOpen {
			_ = stageRoot.Close()
		}
		// Once renamed, stageName is no longer ours.  A same-user concurrent
		// process could create that randomly named entry before this deferred
		// cleanup runs; never remove a replacement after publication.
		if !stagePublished {
			_ = rootHandle.RemoveAll(stageName)
		}
	}()

	if err := writeMarker(stageRoot); err != nil {
		return Project{}, fmt.Errorf("sheets init %q: create marker: %w", path, err)
	}
	stagePath := filepath.Join(root, stageName)
	database, err := store.Open(ctx, filepath.Join(stagePath, DatabaseFileName))
	if err != nil {
		return Project{}, fmt.Errorf("sheets init %q: initialize database: %w", path, err)
	}
	if err := database.Close(); err != nil {
		return Project{}, fmt.Errorf("sheets init %q: close initialized database: %w", path, err)
	}
	if err := stageRoot.Chmod(DatabaseFileName, 0600); err != nil {
		return Project{}, fmt.Errorf("sheets init %q: secure database permissions: %w", path, err)
	}
	if dir, openErr := stageRoot.Open("."); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	if err := stageRoot.Close(); err != nil {
		return Project{}, fmt.Errorf("sheets init %q: close staging directory: %w", path, err)
	}
	stageOpen = false
	if err := rootHandle.Rename(stageName, MetadataDirName); err != nil {
		// Another initializer can win the one atomic rename. Its fully staged
		// project is the idempotent result; no partial .sheets is accepted.
		if _, statErr := rootHandle.Lstat(MetadataDirName); statErr == nil {
			return completeExistingProject(ctx, p)
		}
		return Project{}, fmt.Errorf("sheets init %q: publish metadata: %w", path, err)
	}
	stagePublished = true
	if dir, openErr := rootHandle.Open("."); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	if err := validateMetadata(p); err != nil {
		return Project{}, err
	}
	return p, nil
}

func createStage(root *os.Root) (string, *os.Root, error) {
	for attempt := 0; attempt < 128; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := ".sheets-init-" + hex.EncodeToString(random[:])
		if err := root.Mkdir(name, 0700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", nil, err
		}
		opened, err := root.OpenRoot(name)
		if err != nil {
			_ = root.Remove(name)
			return "", nil, err
		}
		return name, opened, nil
	}
	return "", nil, errors.New("could not allocate a unique staging directory")
}

func writeMarker(metadata *os.Root) error {
	marker, err := metadata.OpenFile(MarkerFileName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(marker, markerContents); err == nil {
		err = marker.Sync()
	}
	return errors.Join(err, marker.Close())
}

func completeExistingProject(ctx context.Context, p Project) (Project, error) {
	if err := validateMetadata(p); err != nil {
		return Project{}, err
	}
	database, err := store.Open(ctx, p.DBPath)
	if err != nil {
		return Project{}, fmt.Errorf("initialize sheets database %s: %w", p.DBPath, err)
	}
	checkErr := database.CheckIntegrity(ctx)
	closeErr := database.Close()
	if err := errors.Join(checkErr, closeErr); err != nil {
		return Project{}, fmt.Errorf("validate sheets database %s: %w", p.DBPath, err)
	}
	metadata, err := os.OpenRoot(p.MetadataDir)
	if err != nil {
		return Project{}, fmt.Errorf("secure sheets metadata %s: %w", p.MetadataDir, err)
	}
	defer func() { _ = metadata.Close() }()
	if err := metadata.Chmod(DatabaseFileName, 0600); err != nil {
		return Project{}, fmt.Errorf("secure sheets database %s: %w", p.DBPath, err)
	}
	if _, err := metadata.Lstat(MarkerFileName); os.IsNotExist(err) {
		if err := writeMarker(metadata); err != nil && !os.IsExist(err) {
			return Project{}, fmt.Errorf("create sheets marker: %w", err)
		}
	} else if err != nil {
		return Project{}, fmt.Errorf("inspect sheets marker: %w", err)
	}
	if err := validateMetadata(p); err != nil {
		return Project{}, err
	}
	return p, nil
}

// Discover finds the nearest sheets project at or above start. start may be
// a file or directory. Both start and the resulting project root are returned
// as absolute, symlink-resolved paths.
func Discover(start string) (Project, error) {
	location, err := canonicalExisting(start)
	if err != nil {
		return Project{}, fmt.Errorf("sheets discover %q: %w", start, err)
	}
	info, err := os.Stat(location)
	if err != nil {
		return Project{}, fmt.Errorf("sheets discover %q: %w", start, err)
	}
	if !info.IsDir() {
		location = filepath.Dir(location)
	}

	for {
		p := projectAt(location)
		metadata, statErr := os.Lstat(p.MetadataDir)
		if statErr == nil {
			if metadata.Mode()&os.ModeSymlink != 0 {
				return Project{}, layoutError(p, p.MetadataDir, true, "metadata directory is a symlink")
			}
			if !metadata.IsDir() {
				return Project{}, layoutError(p, p.MetadataDir, true, "metadata path is not a directory")
			}
			if err := validateMetadata(p); err != nil {
				return Project{}, err
			}
			return p, nil
		}
		if !os.IsNotExist(statErr) {
			return Project{}, fmt.Errorf("sheets discover %q: inspect %s: %w", start, p.MetadataDir, statErr)
		}
		parent := filepath.Dir(location)
		if parent == location {
			return Project{}, fmt.Errorf("sheets discover %q: %w", start, ErrProjectNotFound)
		}
		location = parent
	}
}

func canonicalDirectory(path string) (string, error) {
	location, err := canonicalExisting(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(location)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	return location, nil
}

// canonicalInitDirectory follows Git's convenient behavior of creating a
// missing target directory. Existing paths are still checked strictly and a
// regular file is never replaced.
func canonicalInitDirectory(path string) (string, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); os.IsNotExist(err) {
		if err := os.MkdirAll(abs, 0700); err != nil {
			return "", err
		}
	}
	return canonicalDirectory(abs)
}

func canonicalExisting(path string) (string, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func validateMetadata(p Project) error {
	root, err := os.OpenRoot(p.Root)
	if err != nil {
		return fmt.Errorf("secure sheets root %s: %w", p.Root, err)
	}
	defer func() { _ = root.Close() }()
	expectedMetadata, err := root.Lstat(MetadataDirName)
	if err != nil {
		return fmt.Errorf("inspect sheets metadata %s: %w", p.MetadataDir, err)
	}
	if expectedMetadata.Mode()&os.ModeSymlink != 0 || !expectedMetadata.IsDir() {
		return layoutError(p, p.MetadataDir, true, "metadata path is not a real directory")
	}
	if unsafePermissions(expectedMetadata.Mode()) {
		return layoutError(p, p.MetadataDir, true, "metadata directory is group- or world-writable")
	}
	metadata, err := root.OpenRoot(MetadataDirName)
	if err != nil {
		return layoutError(p, p.MetadataDir, true, "metadata directory changed during validation")
	}
	defer func() { _ = metadata.Close() }()
	actualMetadata, err := metadata.Stat(".")
	if err != nil || !os.SameFile(expectedMetadata, actualMetadata) {
		return layoutError(p, p.MetadataDir, true, "metadata directory changed during validation")
	}

	markerPath := filepath.Join(p.MetadataDir, MarkerFileName)
	markerInfo, markerErr := metadata.Lstat(MarkerFileName)
	if markerErr == nil {
		marker, err := openSecureRegular(metadata, MarkerFileName, markerInfo)
		if err != nil {
			return layoutError(p, markerPath, true, "marker changed during validation")
		}
		contents, readErr := io.ReadAll(io.LimitReader(marker, int64(len(markerContents)+1)))
		readErr = errors.Join(readErr, marker.Close())
		if readErr != nil {
			return fmt.Errorf("read sheets marker %s: %w", markerPath, readErr)
		}
		if !bytes.Equal(contents, []byte(markerContents)) {
			return layoutError(p, markerPath, false, "marker has an unrecognized format")
		}
	} else if !os.IsNotExist(markerErr) {
		return fmt.Errorf("inspect sheets marker %s: %w", markerPath, markerErr)
	}

	dbPath := filepath.Join(p.MetadataDir, DatabaseFileName)
	dbInfo, dbErr := metadata.Lstat(DatabaseFileName)
	if dbErr == nil {
		file, err := openSecureRegular(metadata, DatabaseFileName, dbInfo)
		if err != nil {
			return layoutError(p, dbPath, true, "database changed during validation")
		}
		if dbInfo.Size() > 0 {
			header := make([]byte, 16)
			_, readErr := io.ReadFull(file, header)
			readErr = errors.Join(readErr, file.Close())
			if readErr != nil || !bytes.Equal(header, []byte("SQLite format 3\x00")) {
				return layoutError(p, dbPath, false, "database is not a SQLite database")
			}
		} else if err := file.Close(); err != nil {
			return fmt.Errorf("close sheets database %s: %w", dbPath, err)
		}
	} else if !os.IsNotExist(dbErr) {
		return fmt.Errorf("inspect sheets database %s: %w", dbPath, dbErr)
	}

	if markerErr != nil && dbErr != nil {
		return layoutError(p, p.MetadataDir, false, "missing project marker and database")
	}
	if markerErr != nil && dbInfo.Size() == 0 {
		return layoutError(p, dbPath, false, "database is empty")
	}
	return nil
}

func openSecureRegular(root *os.Root, name string, expected os.FileInfo) (*os.File, error) {
	if expected.Mode()&os.ModeSymlink != 0 || !expected.Mode().IsRegular() || unsafePermissions(expected.Mode()) {
		return nil, ErrUnsafeLayout
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrUnsafeLayout
	}
	return file, nil
}

func unsafePermissions(mode os.FileMode) bool {
	return runtime.GOOS != "windows" && mode.Perm()&0022 != 0
}

func layoutError(p Project, path string, unsafe bool, detail string) error {
	err := ErrInvalidLayout
	if unsafe {
		err = ErrUnsafeLayout
	}
	return &LayoutError{Root: p.Root, Path: path, Detail: detail, Err: err}
}
