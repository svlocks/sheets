// Package project contains sheets project initialization and discovery.
package project

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// Init creates a sheets metadata directory at path. It is idempotent for an
// existing valid project (including a project represented by an initialized
// sheets.db). Init does not create the database; the store creates it later.
func Init(path string) (Project, error) {
	root, err := canonicalInitDirectory(path)
	if err != nil {
		return Project{}, fmt.Errorf("sheets init %q: %w", path, err)
	}
	p := projectAt(root)

	info, err := os.Lstat(p.MetadataDir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return Project{}, layoutError(p, p.MetadataDir, true, "metadata directory is a symlink")
		}
		if !info.IsDir() {
			return Project{}, layoutError(p, p.MetadataDir, true, "metadata path is not a directory")
		}
		if err := validateMetadata(p); err != nil {
			return Project{}, err
		}
		return p, nil
	}
	if !os.IsNotExist(err) {
		return Project{}, fmt.Errorf("sheets init %q: inspect metadata: %w", path, err)
	}

	// Mkdir can race with another initializer. In that case validate the
	// resulting directory and return it as an idempotent initialization.
	if err := os.Mkdir(p.MetadataDir, 0700); err != nil {
		if !os.IsExist(err) {
			return Project{}, fmt.Errorf("sheets init %q: create metadata: %w", path, err)
		}
		info, statErr := os.Lstat(p.MetadataDir)
		if statErr != nil {
			return Project{}, fmt.Errorf("sheets init %q: inspect metadata after race: %w", path, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return Project{}, layoutError(p, p.MetadataDir, true, "metadata path is not a directory")
		}
		if err := validateMetadata(p); err != nil {
			return Project{}, err
		}
		return p, nil
	}

	marker, err := os.OpenFile(filepath.Join(p.MetadataDir, MarkerFileName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			if validateErr := validateMetadata(p); validateErr == nil {
				return p, nil
			}
			// A concurrent initializer (or caller) owns an existing marker;
			// never remove its metadata while reporting its invalid layout.
			return Project{}, fmt.Errorf("sheets init %q: create marker: %w", path, err)
		}
		// Do not clean up if a marker appeared concurrently or cannot be
		// inspected due to permissions.
		if _, statErr := os.Lstat(filepath.Join(p.MetadataDir, MarkerFileName)); os.IsNotExist(statErr) {
			_ = os.Remove(p.MetadataDir)
		}
		return Project{}, fmt.Errorf("sheets init %q: create marker: %w", path, err)
	}
	if _, err = io.WriteString(marker, markerContents); err == nil {
		err = marker.Sync()
	}
	closeErr := marker.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		// Only the marker was created by this invocation, so cleanup cannot
		// remove user data from a pre-existing project.
		_ = os.Remove(filepath.Join(p.MetadataDir, MarkerFileName))
		_ = os.Remove(p.MetadataDir)
		return Project{}, fmt.Errorf("sheets init %q: write marker: %w", path, err)
	}
	// Sync is not supported for directories on every platform; the marker's
	// own Sync above still gives durable contents where directory sync exists.
	if dir, openErr := os.Open(p.MetadataDir); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
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
	markerPath := filepath.Join(p.MetadataDir, MarkerFileName)
	dbPath := filepath.Join(p.MetadataDir, DatabaseFileName)

	marker, markerErr := os.Lstat(markerPath)
	if markerErr == nil {
		if marker.Mode()&os.ModeSymlink != 0 {
			return layoutError(p, markerPath, true, "marker is a symlink")
		}
		if !marker.Mode().IsRegular() {
			return layoutError(p, markerPath, true, "marker is not a regular file")
		}
		contents, err := os.ReadFile(markerPath)
		if err != nil {
			return fmt.Errorf("read sheets marker %s: %w", markerPath, err)
		}
		if !bytes.Equal(contents, []byte(markerContents)) {
			return layoutError(p, markerPath, false, "marker has an unrecognized format")
		}
	} else if !os.IsNotExist(markerErr) {
		return fmt.Errorf("inspect sheets marker %s: %w", markerPath, markerErr)
	}

	db, dbErr := os.Lstat(dbPath)
	if dbErr == nil {
		if db.Mode()&os.ModeSymlink != 0 {
			return layoutError(p, dbPath, true, "database is a symlink")
		}
		if !db.Mode().IsRegular() {
			return layoutError(p, dbPath, true, "database is not a regular file")
		}
		if db.Size() > 0 {
			file, err := os.Open(dbPath)
			if err != nil {
				return fmt.Errorf("read sheets database %s: %w", dbPath, err)
			}
			header := make([]byte, 16)
			_, readErr := io.ReadFull(file, header)
			_ = file.Close()
			if readErr != nil || !bytes.Equal(header, []byte("SQLite format 3\x00")) {
				return layoutError(p, dbPath, false, "database is not a SQLite database")
			}
		}
	} else if !os.IsNotExist(dbErr) {
		return fmt.Errorf("inspect sheets database %s: %w", dbPath, dbErr)
	}

	if markerErr != nil && dbErr != nil {
		return layoutError(p, p.MetadataDir, false, "missing project marker and database")
	}
	if markerErr != nil && db.Size() == 0 {
		return layoutError(p, dbPath, false, "database is empty")
	}
	return nil
}

func layoutError(p Project, path string, unsafe bool, detail string) error {
	err := ErrInvalidLayout
	if unsafe {
		err = ErrUnsafeLayout
	}
	return &LayoutError{Root: p.Root, Path: path, Detail: detail, Err: err}
}
