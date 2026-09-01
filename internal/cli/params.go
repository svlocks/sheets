package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/svlocks/sheets/internal/app"
)

const maxInputBytes int64 = 64 << 20

// ErrInputTooLarge identifies query or parameter input that exceeds the
// explicit frontend allocation bound.
var ErrInputTooLarge = errors.New("CLI input exceeds size limit")

// parameterInput describes the two CLI parameter forms. Object accepts a JSON
// object, @path, or - for stdin. Values contains name=JSON assignments.
type parameterInput struct {
	Object string
	Values []string
}

func (p parameterInput) load(stdin io.Reader) (map[string]any, error) {
	return p.loadContext(context.Background(), stdin)
}

func (p parameterInput) loadContext(ctx context.Context, stdin io.Reader) (map[string]any, error) {
	if ctx == nil {
		return nil, errors.New("load parameters: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params := make(map[string]any)
	if p.Object != "" {
		data, err := readParameterSource(ctx, p.Object, stdin)
		if err != nil {
			return nil, err
		}
		if err := decodeJSON(data, &params); err != nil {
			return nil, fmt.Errorf("decode --params: %w", err)
		}
		if params == nil {
			return nil, fmt.Errorf("decode --params: expected a JSON object, got null")
		}
	}

	for _, assignment := range p.Values {
		name, value, ok := strings.Cut(assignment, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid --param %q: expected name=JSON", assignment)
		}
		if _, exists := params[name]; exists {
			return nil, fmt.Errorf("parameter %q was supplied more than once", name)
		}
		var decoded any
		if err := decodeJSON([]byte(value), &decoded); err != nil {
			return nil, fmt.Errorf("decode --param %s: %w", name, err)
		}
		params[name] = decoded
	}
	return params, nil
}

func readParameterSource(ctx context.Context, source string, stdin io.Reader) ([]byte, error) {
	switch {
	case source == "-":
		data, err := readAllContext(ctx, stdin)
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if err != nil {
			return nil, fmt.Errorf("read parameters from stdin: %w", err)
		}
		return data, nil
	case strings.HasPrefix(source, "@"):
		if len(source) == 1 {
			return nil, fmt.Errorf("parameter file path is empty")
		}
		data, err := readFileContext(ctx, source[1:])
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if err != nil {
			return nil, fmt.Errorf("read parameter file: %w", err)
		}
		return data, nil
	default:
		return []byte(source), nil
	}
}

type readAllResult struct {
	data []byte
	err  error
}

// readAllContext allows process cancellation to win over a reader that does
// not itself accept a context. The result channel is buffered so a reader that
// eventually unblocks can finish without retaining command-owned state.
func readAllContext(ctx context.Context, reader io.Reader) ([]byte, error) {
	return readAllContextLimit(ctx, reader, maxInputBytes)
}

func readFileContext(ctx context.Context, path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := readAllContext(ctx, file)
	return data, errors.Join(readErr, file.Close())
}

func readAllContextLimit(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("read input: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, errors.New("read input: nil reader")
	}
	if limit < 0 {
		return nil, errors.New("read input: negative size limit")
	}
	result := make(chan readAllResult, 1)
	go func() {
		limited := &io.LimitedReader{R: reader, N: limit + 1}
		data, err := io.ReadAll(limited)
		if err == nil && int64(len(data)) > limit {
			data = nil
			err = fmt.Errorf("%w (%d bytes)", ErrInputTooLarge, limit)
		}
		result <- readAllResult{data: data, err: err}
	}()
	select {
	case value := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return value.data, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func decodeJSON(data []byte, dst any) error {
	if err := rejectDuplicateObjectKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("unexpected content after JSON value")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected content after JSON value")
		}
		return err
	}
	return normalizeJSONNumbers(dst)
}

func rejectDuplicateObjectKeys(data []byte) error {
	return app.RejectDuplicateJSONKeys(data)
}

// normalizeJSONNumbers changes json.Number values in decoded data into int64
// or float64. This preserves integer semantics expected by Cypher.
func normalizeJSONNumbers(dst any) error {
	switch value := dst.(type) {
	case *map[string]any:
		if *value == nil {
			return nil
		}
		normalized, err := normalizeMap(*value)
		if err != nil {
			return err
		}
		decoded, err := app.DecodeTaggedJSONValue(normalized, false)
		if err != nil {
			return err
		}
		result, ok := decoded.(map[string]any)
		if !ok {
			return fmt.Errorf("expected a JSON object, got a typed value")
		}
		*value = result
		return nil
	case *any:
		normalized, err := normalizeJSONValue(*value)
		if err != nil {
			return err
		}
		decoded, err := app.DecodeTaggedJSONValue(normalized, false)
		if err != nil {
			return err
		}
		*value = decoded
		return nil
	default:
		return fmt.Errorf("unsupported JSON destination %T", dst)
	}
}

func normalizeMap(input map[string]any) (map[string]any, error) {
	for key, value := range input {
		normalized, err := normalizeJSONValue(value)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", key, err)
		}
		input[key] = normalized
	}
	return input, nil
}

func normalizeJSONValue(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		raw := value.String()
		if !strings.ContainsAny(raw, ".eE") {
			integer, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("integer %q is outside Cypher's signed 64-bit range", raw)
			}
			return integer, nil
		}
		decimal, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", raw)
		}
		return decimal, nil
	case []any:
		for index, item := range value {
			normalized, err := normalizeJSONValue(item)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			value[index] = normalized
		}
		return value, nil
	case map[string]any:
		return normalizeMap(value)
	default:
		return value, nil
	}
}
