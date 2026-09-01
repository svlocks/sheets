package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	graphstore "github.com/svlocks/sheets/internal/store"
)

func TestInitAndDiscover(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	p, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	wantRoot, _ := filepath.EvalSymlinks(root)
	if p.Root != wantRoot || p.MetadataDir != filepath.Join(wantRoot, MetadataDirName) || p.DBPath != filepath.Join(wantRoot, MetadataDirName, DatabaseFileName) {
		t.Fatalf("unexpected project paths: %#v", p)
	}
	if _, err := os.Stat(filepath.Join(p.MetadataDir, MarkerFileName)); err != nil {
		t.Fatalf("marker was not created: %v", err)
	}
	databaseInfo, err := os.Stat(p.DBPath)
	if err != nil || !databaseInfo.Mode().IsRegular() || databaseInfo.Size() == 0 {
		t.Fatalf("Init did not create an initialized database: %v, %#v", err, databaseInfo)
	}
	if runtime.GOOS != "windows" && databaseInfo.Mode().Perm()&0022 != 0 {
		t.Fatalf("database permissions are unsafe: %o", databaseInfo.Mode().Perm())
	}

	child := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	found, err := Discover(child)
	if err != nil || found != p {
		t.Fatalf("Discover child = %#v, %v; want %#v", found, err, p)
	}
	again, err := Init(root)
	if err != nil || again != p {
		t.Fatalf("idempotent Init = %#v, %v; want %#v", again, err, p)
	}
}

func TestInitCreatesMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new", "project")
	p, err := Init(root)
	if err != nil {
		t.Fatalf("Init missing root: %v", err)
	}
	if _, err := os.Stat(p.MetadataDir); err != nil {
		t.Fatalf("metadata directory was not created: %v", err)
	}
}

func TestDiscoverFileAndSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	p, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "nested", "note.txt")
	if err := os.WriteFile(file, []byte("note"), 0644); err != nil {
		t.Fatal(err)
	}
	found, err := Discover(file)
	if err != nil || found != p {
		t.Fatalf("Discover file = %#v, %v; want %#v", found, err, p)
	}
	link := filepath.Join(base, "alias")
	if err := os.Symlink(root, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	found, err = Discover(filepath.Join(link, "nested"))
	if err != nil || found != p {
		t.Fatalf("Discover symlink = %#v, %v; want %#v", found, err, p)
	}
}

func TestDiscoverNearestNestedProject(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0755); err != nil {
		t.Fatal(err)
	}
	out, err := Init(outer)
	if err != nil {
		t.Fatal(err)
	}
	in, err := Init(inner)
	if err != nil {
		t.Fatal(err)
	}
	found, err := Discover(filepath.Join(inner, "deep"))
	if err == nil {
		t.Fatalf("Discover nonexistent child unexpectedly succeeded: %#v", found)
	}
	if err := os.Mkdir(filepath.Join(inner, "deep"), 0755); err != nil {
		t.Fatal(err)
	}
	found, err = Discover(filepath.Join(inner, "deep"))
	if err != nil || found != in {
		t.Fatalf("Discover nested = %#v, %v; want %#v", found, err, in)
	}
	found, err = Discover(outer)
	if err != nil || found != out {
		t.Fatalf("Discover outer = %#v, %v; want %#v", found, err, out)
	}
}

func TestDiscoverDatabaseOnlyLayout(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, MetadataDirName)
	if err := os.Mkdir(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := graphstore.Open(context.Background(), filepath.Join(metadata, DatabaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	found, discoverErr := Discover(root)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := projectAt(canonicalRoot)
	if discoverErr != nil || found != want {
		t.Fatalf("Discover DB-only = %#v, %v; want %#v", found, discoverErr, want)
	}
}

func TestConcurrentInitPublishesOnlyCompleteProject(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	const workers = 24
	start := make(chan struct{})
	results := make(chan Project, workers)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			project, err := Init(root)
			if err != nil {
				errorsCh <- err
				return
			}
			results <- project
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent Init: %v", err)
	}
	var expected Project
	for result := range results {
		if expected == (Project{}) {
			expected = result
		}
		if result != expected {
			t.Errorf("Init results differ: %#v != %#v", result, expected)
		}
	}
	if expected == (Project{}) {
		t.Fatal("no initializer succeeded")
	}
	if _, err := Discover(root); err != nil {
		t.Fatalf("published project is not discoverable: %v", err)
	}
	if database, err := graphstore.Open(context.Background(), expected.DBPath); err != nil {
		t.Fatalf("published database is not openable: %v", err)
	} else if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != MetadataDirName {
			t.Fatalf("initializer leaked staging entry %q", entry.Name())
		}
	}
}

func TestInitContextCancellationDoesNotPublishPartialMetadata(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InitContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("InitContext error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(root, MetadataDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled initialization published metadata: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled initialization leaked entries: %#v", entries)
	}
}

func TestDiscoverIgnoresUnpublishedStagingDirectory(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, ".sheets-init-interrupted")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, MarkerFileName), []byte(markerContents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("Discover staging-only layout error = %v", err)
	}
}

func TestRejectsCorruptAndUnsafeLayouts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(string) error
		want  error
	}{
		{"bad marker", func(root string) error {
			metadata := filepath.Join(root, MetadataDirName)
			return os.MkdirAll(metadata, 0700)
		}, ErrInvalidLayout},
		{"metadata symlink", func(root string) error {
			outside := filepath.Join(filepath.Dir(root), "outside")
			if err := os.Mkdir(outside, 0700); err != nil {
				return err
			}
			return os.Symlink(outside, filepath.Join(root, MetadataDirName))
		}, ErrUnsafeLayout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := test.setup(root); err != nil {
				if runtime.GOOS == "windows" && test.name == "metadata symlink" {
					t.Skipf("symlinks unavailable: %v", err)
				}
				t.Fatal(err)
			}
			if test.name == "bad marker" {
				if err := os.WriteFile(filepath.Join(root, MetadataDirName, MarkerFileName), []byte("wrong"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Discover(root)
			if !errors.Is(err, test.want) {
				t.Fatalf("Discover error = %v; errors.Is(_, %v) is false", err, test.want)
			}
		})
	}
}

func TestDiscoverNotFoundAtFilesystemRoot(t *testing.T) {
	_, err := Discover(t.TempDir())
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("Discover error = %v; errors.Is(_, ErrProjectNotFound) is false", err)
	}
}
