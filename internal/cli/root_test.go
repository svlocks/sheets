package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/engine"
	"github.com/svlocks/sheets/internal/project"
)

func runCommand(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	command := New(Options{})
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetIn(strings.NewReader(stdin))
	command.SetArgs(args)
	err := command.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func initializeProject(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace")
	stdout, _, err := runCommand(t, "", "init", "--quiet", root)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("quiet init output = %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(root, project.MetadataDirName, project.DatabaseFileName)); err != nil {
		t.Fatalf("database missing: %v", err)
	}
	return root
}

func TestInitRootAndNestedDiscovery(t *testing.T) {
	root := initializeProject(t)
	nested := filepath.Join(root, "src", "module")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCommand(t, "", "-C", nested, "root")
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(root)
	if got := strings.TrimSpace(stdout); got != canonical {
		t.Fatalf("root = %q, want %q", got, canonical)
	}

	stdout, _, err = runCommand(t, "", "-C", nested, "root", "--database")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(stdout), filepath.Join(canonical, ".sheets", "sheets.db"); got != want {
		t.Fatalf("database = %q, want %q", got, want)
	}
}

func TestInitHonorsCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cancelled")
	command := New(Options{})
	command.SetArgs([]string{"init", root})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := command.ExecuteContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("init error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, project.MetadataDirName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("metadata stat error = %v, want not exist", statErr)
	}
}

func TestExecAndQueryJSON(t *testing.T) {
	root := initializeProject(t)
	stdout, _, err := runCommand(t, "", "-C", root, "exec", "--output", "json",
		"--param", `title="Integrate SDK"`,
		"CREATE (n:Task {title:$title}) RETURN n.title AS title")
	if err != nil {
		t.Fatal(err)
	}
	var created app.BatchResult
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if created.Revision == nil || *created.Revision != 1 || created.Results[0].Rows[0][0] != "Integrate SDK" {
		t.Fatalf("created = %#v", created)
	}

	stdout, _, err = runCommand(t, "", "-C", root, "query", "--output", "json",
		"MATCH (n:Task) RETURN n.title AS title")
	if err != nil {
		t.Fatal(err)
	}
	var queried app.BatchResult
	if err := json.Unmarshal([]byte(stdout), &queried); err != nil {
		t.Fatal(err)
	}
	if got := queried.Results[0].Rows; len(got) != 1 || got[0][0] != "Integrate SDK" {
		t.Fatalf("query rows = %#v", got)
	}
}

func TestQueryJSONLStreamsAndExecJSONLPreservesAtomicRevision(t *testing.T) {
	root := initializeProject(t)
	created, _, err := runCommand(t, "", "-C", root, "exec", "--output", "jsonl",
		"CREATE (:Task {title:'created'}) RETURN 1 AS value")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{`"type":"row"`, `"type":"summary"`, `"type":"revision"`} {
		if !strings.Contains(created, record) {
			t.Fatalf("atomic exec JSONL lacks %s: %s", record, created)
		}
	}

	streamed, _, err := runCommand(t, "", "-C", root, "query", "--output", "jsonl",
		"UNWIND range(1, 3) AS value RETURN value")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(streamed, `"type":"row"`); got != 3 {
		t.Fatalf("streamed row records = %d, want 3: %s", got, streamed)
	}

	partial, _, err := runCommand(t, "", "-C", root, "query", "--output", "jsonl",
		"UNWIND [1, 'bad'] AS value RETURN value + 1 AS result")
	if err == nil {
		t.Fatal("runtime-error JSONL query succeeded")
	}
	if got := strings.Count(partial, `"type":"row"`); got != 1 {
		t.Fatalf("runtime-error JSONL valid prefix has %d rows, want 1: %s (error %v)", got, partial, err)
	}
}

func TestExecFileIsOneAtomicRevision(t *testing.T) {
	root := initializeProject(t)
	file := filepath.Join(t.TempDir(), "batch.cypher")
	if err := os.WriteFile(file, []byte("CREATE (:Task {title:'one'}); CREATE (:Task {title:'two'});"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCommand(t, "", "-C", root, "exec", "--file", file, "--output", "json", "--actor", "test-agent", "--message", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var batch app.BatchResult
	if err := json.Unmarshal([]byte(stdout), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Revision == nil || *batch.Revision != 1 || len(batch.Results) != 2 {
		t.Fatalf("batch = %#v", batch)
	}

	history, _, err := runCommand(t, "", "-C", root, "history", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(history, "test-agent") || !strings.Contains(history, "seed") {
		t.Fatalf("history = %s", history)
	}
}

func TestHistoryDescendingPagination(t *testing.T) {
	root := initializeProject(t)
	for index := 1; index <= 3; index++ {
		query := fmt.Sprintf("CREATE (:Task {number:%d})", index)
		if _, _, err := runCommand(t, "", "-C", root, "exec", query); err != nil {
			t.Fatal(err)
		}
	}
	firstJSON, _, err := runCommand(t, "", "-C", root, "history", "--order", "descending", "--limit", "2", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var first app.BatchResult
	if err := json.Unmarshal([]byte(firstJSON), &first); err != nil {
		t.Fatal(err)
	}
	if got := first.Results[0].Rows; len(got) != 2 || got[0][0] != float64(3) || got[1][0] != float64(2) || first.Results[0].Page == nil || first.Results[0].Page.Next == "" {
		t.Fatalf("first descending history page = %#v", first)
	}
	secondJSON, _, err := runCommand(t, "", "-C", root, "history", "--order", "desc", "--limit", "2", "--after", first.Results[0].Page.Next, "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var second app.BatchResult
	if err := json.Unmarshal([]byte(secondJSON), &second); err != nil {
		t.Fatal(err)
	}
	if got := second.Results[0].Rows; len(got) != 1 || got[0][0] != float64(1) || second.Results[0].Page != nil {
		t.Fatalf("second descending history page = %#v", second)
	}
}

func TestQueryRejectsMutationAndExecRejectsHistoricalWrite(t *testing.T) {
	root := initializeProject(t)
	_, _, err := runCommand(t, "", "-C", root, "query", "CREATE (:Task)")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("query mutation error = %v", err)
	}
	_, _, err = runCommand(t, "", "-C", root, "exec", "--at-revision", "0", "CREATE (:Task)")
	if err == nil || !strings.Contains(err.Error(), "historical") {
		t.Fatalf("historical mutation error = %v", err)
	}
}

func TestQueryReadsCypherAndParametersFromFiles(t *testing.T) {
	root := initializeProject(t)
	directory := t.TempDir()
	queryPath := filepath.Join(directory, "query.cypher")
	paramsPath := filepath.Join(directory, "params.json")
	if err := os.WriteFile(queryPath, []byte("RETURN $nested.count AS count"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paramsPath, []byte(`{"nested":{"count":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCommand(t, "", "-C", root, "query", "--file", queryPath, "--params", "@"+paramsPath, "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "7") {
		t.Fatalf("output = %s", stdout)
	}
}

func TestStatusReportsGraphCounts(t *testing.T) {
	root := initializeProject(t)
	if _, _, err := runCommand(t, "", "-C", root, "exec", "CREATE (:Task)-[:BLOCKS]->(:Task)"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCommand(t, "", "-C", root, "status", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"revision"`) || !strings.Contains(stdout, `"relationships"`) {
		t.Fatalf("status = %s", stdout)
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}

func TestCancellationInterruptsBlockedStdin(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "query", args: []string{"query", "--file", "-"}},
		{name: "parameters", args: []string{"query", "RETURN $value", "--params", "-"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
			command := New(Options{})
			command.SetIn(reader)
			command.SetArgs(test.args)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- command.ExecuteContext(ctx) }()
			select {
			case <-reader.started:
			case <-time.After(2 * time.Second):
				t.Fatal("command did not start reading stdin")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("command error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("command remained blocked after cancellation")
			}
			close(reader.release)
		})
	}
}

func TestTUILaunchesDiscoveredProjectFromNestedDirectory(t *testing.T) {
	root := initializeProject(t)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	called := false
	command := New(Options{TUI: func(ctx context.Context, found project.Project, executor *engine.Engine, options TUIOptions) error {
		called = true
		if found.Root != canonicalRoot {
			t.Fatalf("TUI project root = %q, want %q", found.Root, canonicalRoot)
		}
		if _, err := executor.Execute(ctx, app.ExecuteRequest{Query: "RETURN 1", ReadOnly: true}); err != nil {
			t.Fatalf("TUI executor: %v", err)
		}
		if !options.NoColor {
			t.Fatal("TUI no-color option was not forwarded")
		}
		return nil
	}})
	command.SetArgs([]string{"-C", nested, "tui", "--no-color"})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("TUI runner was not called")
	}
}

func TestTUIHonorsEmptyNoColorEnvironment(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	root := initializeProject(t)
	called := false
	command := New(Options{TUI: func(_ context.Context, _ project.Project, _ *engine.Engine, options TUIOptions) error {
		called = true
		if !options.NoColor {
			t.Fatal("NO_COLOR presence was ignored when its value was empty")
		}
		return nil
	}})
	command.SetArgs([]string{"-C", root, "tui"})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("TUI runner was not called")
	}
}

func TestInvalidOutputFailsBeforeProjectDiscovery(t *testing.T) {
	tests := [][]string{
		{"history", "--output", "yaml"},
		{"status", "--output", "yaml"},
		{"query", "--output", "yaml", "RETURN 1"},
		{"exec", "--output", "yaml", "RETURN 1"},
	}
	for _, args := range tests {
		_, _, err := runCommand(t, "", append([]string{"-C", filepath.Join(t.TempDir(), "missing")}, args...)...)
		if !errors.Is(err, ErrInvalidFormat) {
			t.Fatalf("%s error = %v, want ErrInvalidFormat", args[0], err)
		}
	}
}
