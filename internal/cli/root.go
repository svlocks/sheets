package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/engine"
	"github.com/svlocks/sheets/internal/project"
	"github.com/svlocks/sheets/internal/store"
	"github.com/spf13/cobra"
)

// TUIOptions contains terminal presentation settings owned by the CLI.
type TUIOptions struct {
	NoColor bool
}

// TUIRunner launches the interactive frontend for an already-open project.
type TUIRunner func(context.Context, project.Project, *engine.Engine, TUIOptions) error

// Options provides process integrations while keeping commands testable.
type Options struct {
	TUI TUIRunner
}

type commandEnvironment struct {
	start string
	tui   TUIRunner
}

// New constructs the complete sheets command tree without executing it.
func New(options Options) *cobra.Command {
	environment := &commandEnvironment{start: ".", tui: options.TUI}
	root := &cobra.Command{
		Use:           "sheets",
		Short:         "A temporal property graph for tracking work",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
	}
	root.PersistentFlags().StringVarP(&environment.start, "directory", "C", ".", "start project discovery or initialization from `path`")
	root.AddCommand(
		environment.initCommand(),
		environment.rootCommand(),
		environment.queryCommand(true),
		environment.queryCommand(false),
		environment.historyCommand(),
		environment.statusCommand(),
		environment.skillCommand(),
		environment.tuiCommand(),
	)
	return root
}

func (e *commandEnvironment) initCommand() *cobra.Command {
	var quiet bool
	command := &cobra.Command{
		Use:   "init [directory]",
		Short: "Initialize a sheets project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			target := e.start
			if len(args) == 1 {
				target = args[0]
				if !filepath.IsAbs(target) && e.start != "." {
					base, err := filepath.Abs(e.start)
					if err != nil {
						return err
					}
					target = filepath.Join(base, target)
				}
			}
			found, err := project.InitContext(command.Context(), target)
			if err != nil {
				return err
			}
			if !quiet {
				_, err = fmt.Fprintf(command.OutOrStdout(), "Initialized sheets project in %s\n", found.MetadataDir)
			}
			return err
		},
	}
	command.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress success output")
	return command
}

func (e *commandEnvironment) rootCommand() *cobra.Command {
	var metadata, database bool
	command := &cobra.Command{
		Use:   "root",
		Short: "Print the nearest sheets project root",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			found, err := project.Discover(e.start)
			if err != nil {
				return err
			}
			path := found.Root
			if metadata {
				path = found.MetadataDir
			}
			if database {
				path = found.DBPath
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), path)
			return err
		},
	}
	command.Flags().BoolVar(&metadata, "metadata", false, "print the metadata directory")
	command.Flags().BoolVar(&database, "database", false, "print the database path")
	command.MarkFlagsMutuallyExclusive("metadata", "database")
	return command
}

type queryFlags struct {
	file       string
	params     parameterInput
	format     string
	revision   string
	atTime     string
	actor      string
	message    string
	readOnly   bool
	commandUse string
}

func (e *commandEnvironment) queryCommand(readOnly bool) *cobra.Command {
	flags := queryFlags{format: string(FormatTable), readOnly: readOnly}
	use, short := "exec [cypher]", "Execute Cypher reads and mutations atomically"
	if readOnly {
		use, short = "query [cypher]", "Run a read-only Cypher query"
	}
	flags.commandUse = strings.Fields(use)[0]
	command := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ArbitraryArgs,
		RunE: func(command *cobra.Command, args []string) (runErr error) {
			format, err := ParseFormat(flags.format)
			if err != nil {
				return err
			}
			snapshot, err := flags.snapshot()
			if err != nil {
				return err
			}
			query, err := flags.loadQuery(command.Context(), args, command.InOrStdin())
			if err != nil {
				return err
			}
			params, err := flags.params.loadContext(command.Context(), command.InOrStdin())
			if err != nil {
				return err
			}
			_, database, executor, err := e.open(command.Context())
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, database.Close()) }()
			request := app.ExecuteRequest{
				Query: query, Params: params, Snapshot: snapshot, ReadOnly: readOnly,
				Actor: flags.actor, Message: flags.message,
			}
			if format == FormatJSONL {
				stream, streamErr := newJSONLStream(command.OutOrStdout())
				if streamErr != nil {
					return streamErr
				}
				streamErr = executor.ExecuteStream(command.Context(), request, stream.Emit)
				flushErr := stream.Flush()
				if !errors.Is(streamErr, app.ErrStreamingMutation) {
					return errors.Join(streamErr, flushErr)
				}
				// Mutations retain execute-then-render semantics so no result is
				// visible until the transaction commits successfully.
				if flushErr != nil {
					return flushErr
				}
			}
			batch, err := executor.Execute(command.Context(), request)
			if err != nil {
				return err
			}
			return Render(command.OutOrStdout(), string(format), batch)
		},
	}
	command.Flags().StringVarP(&flags.file, "file", "f", "", "read Cypher from `path` (- for stdin)")
	command.Flags().StringVar(&flags.params.Object, "params", "", "JSON object, @file, or - for stdin")
	command.Flags().StringArrayVarP(&flags.params.Values, "param", "p", nil, "set one parameter as name=JSON (repeatable)")
	command.Flags().StringVarP(&flags.format, "output", "o", flags.format, "output format: table, json, or jsonl")
	command.Flags().StringVar(&flags.revision, "at-revision", "", "read graph state at `revision`")
	command.Flags().StringVar(&flags.atTime, "at-time", "", "read graph state at RFC3339 `time`")
	command.MarkFlagsMutuallyExclusive("at-revision", "at-time")
	if !readOnly {
		command.Flags().StringVar(&flags.actor, "actor", os.Getenv("SHEETS_ACTOR"), "revision actor")
		command.Flags().StringVarP(&flags.message, "message", "m", "", "revision message")
	}
	return command
}

func (f queryFlags) loadQuery(ctx context.Context, args []string, stdin io.Reader) (string, error) {
	if ctx == nil {
		return "", errors.New("load query: nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.file != "" && len(args) > 0 {
		return "", fmt.Errorf("query argument and --file are mutually exclusive")
	}
	if f.file == "-" && f.params.Object == "-" {
		return "", fmt.Errorf("query and parameters cannot both read from stdin")
	}
	if f.file != "" {
		var data []byte
		var err error
		if f.file == "-" {
			data, err = readAllContext(ctx, stdin)
		} else {
			data, err = readFileContext(ctx, f.file)
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return "", contextErr
		}
		if err != nil {
			return "", fmt.Errorf("read query: %w", err)
		}
		if strings.TrimSpace(string(data)) == "" {
			return "", fmt.Errorf("query is empty")
		}
		return string(data), nil
	}
	query := strings.Join(args, " ")
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("%s requires Cypher or --file", f.commandUse)
	}
	return query, nil
}

func (f queryFlags) snapshot() (domain.Snapshot, error) {
	if f.revision != "" {
		value, err := strconv.ParseUint(f.revision, 10, 64)
		if err != nil {
			return domain.Snapshot{}, fmt.Errorf("invalid revision %q", f.revision)
		}
		revision := domain.Revision(value)
		return domain.Snapshot{Revision: &revision}, nil
	}
	if f.atTime != "" {
		value, err := time.Parse(time.RFC3339Nano, f.atTime)
		if err != nil {
			return domain.Snapshot{}, fmt.Errorf("invalid RFC3339 time %q: %w", f.atTime, err)
		}
		return domain.Snapshot{Time: &value}, nil
	}
	return domain.Snapshot{}, nil
}

func (e *commandEnvironment) historyCommand() *cobra.Command {
	var limit int
	var after, format, orderText string
	command := &cobra.Command{
		Use:   "history",
		Short: "List committed graph revisions",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (runErr error) {
			parsed, err := ParseFormat(format)
			if err != nil {
				return err
			}
			var order domain.RevisionOrder
			switch strings.ToLower(strings.TrimSpace(orderText)) {
			case "ascending", "asc":
				order = domain.RevisionOrderAscending
			case "descending", "desc":
				order = domain.RevisionOrderDescending
			default:
				return fmt.Errorf("invalid revision order %q: expected ascending or descending", orderText)
			}
			_, database, _, err := e.open(command.Context())
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, database.Close()) }()
			var revisions []domain.RevisionInfo
			var page domain.PageInfo
			if order == domain.RevisionOrderAscending {
				revisions, page, err = database.ListRevisions(command.Context(), domain.Page{Limit: limit, After: after})
			} else {
				revisions, page, err = database.ListRevisionPage(command.Context(), domain.RevisionPage{
					Limit: limit, Cursor: after, Order: order,
				})
			}
			if err != nil {
				return err
			}
			rows := make([][]any, len(revisions))
			for index, revision := range revisions {
				rows[index] = []any{revision.Revision, revision.Time.Format(time.RFC3339Nano), revision.Actor, revision.Message}
			}
			result := app.Result{Columns: []string{"revision", "time", "actor", "message"}, Rows: rows}
			if page.Next != "" {
				result.Page = &page
			}
			batch := app.BatchResult{Results: []app.Result{result}}
			return Render(command.OutOrStdout(), string(parsed), batch)
		},
	}
	command.Flags().IntVar(&limit, "limit", 100, "maximum revisions to return")
	command.Flags().StringVar(&after, "after", "", "opaque pagination cursor")
	command.Flags().StringVar(&orderText, "order", domain.RevisionOrderAscending.String(), "revision order: ascending or descending")
	command.Flags().StringVarP(&format, "output", "o", string(FormatTable), "output format: table, json, or jsonl")
	return command
}

func (e *commandEnvironment) statusCommand() *cobra.Command {
	var format string
	command := &cobra.Command{
		Use:   "status",
		Short: "Show project and current graph statistics",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (runErr error) {
			parsed, err := ParseFormat(format)
			if err != nil {
				return err
			}
			found, database, _, err := e.open(command.Context())
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, database.Close()) }()
			view, err := database.View(command.Context(), domain.Snapshot{})
			if err != nil {
				return err
			}
			nodes, err := view.CountNodes(command.Context(), store.NodePredicate{})
			if err != nil {
				return err
			}
			edges, err := view.CountEdges(command.Context(), store.EdgePredicate{})
			if err != nil {
				return err
			}
			batch := app.BatchResult{Results: []app.Result{{
				Columns: []string{"root", "revision", "nodes", "relationships"},
				Rows:    [][]any{{found.Root, view.Revision(), nodes, edges}},
			}}}
			return Render(command.OutOrStdout(), string(parsed), batch)
		},
	}
	command.Flags().StringVarP(&format, "output", "o", string(FormatTable), "output format: table, json, or jsonl")
	return command
}

func (e *commandEnvironment) tuiCommand() *cobra.Command {
	_, noColorDefault := os.LookupEnv("NO_COLOR")
	var noColor bool
	command := &cobra.Command{
		Use:     "tui",
		Aliases: []string{"ui"},
		Short:   "Open the interactive workspace",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (runErr error) {
			if e.tui == nil {
				return errors.New("this build does not include the terminal UI")
			}
			found, database, executor, err := e.open(command.Context())
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, database.Close()) }()
			return e.tui(command.Context(), found, executor, TUIOptions{NoColor: noColor})
		},
	}
	command.Flags().BoolVar(&noColor, "no-color", noColorDefault, "disable color output")
	return command
}

func (e *commandEnvironment) open(ctx context.Context) (project.Project, *store.Store, *engine.Engine, error) {
	found, err := project.Discover(e.start)
	if err != nil {
		return project.Project{}, nil, nil, err
	}
	database, err := store.Open(ctx, found.DBPath)
	if err != nil {
		return project.Project{}, nil, nil, fmt.Errorf("open sheets database: %w", err)
	}
	executor, err := engine.New(database)
	if err != nil {
		_ = database.Close()
		return project.Project{}, nil, nil, err
	}
	return found, database, executor, nil
}
