package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/store"
)

func TestMutationResourceLimitRollsBackWholeCypherDocument(t *testing.T) {
	executor, database := testEngine(t)
	body := strings.Repeat("x", domain.MaxNodeBodyBytes+1)
	_, err := executor.Execute(context.Background(), app.ExecuteRequest{
		Query:  "CREATE (:Kept {name: 'before'}); CREATE (:TooBig {body: $body})",
		Params: map[string]any{"body": body},
	})
	if !errors.Is(err, store.ErrInvalidArgument) || !errors.Is(err, domain.ErrResourceLimit) {
		t.Fatalf("oversized mutation error = %v", err)
	}
	if revision, revisionErr := database.CurrentRevision(context.Background()); revisionErr != nil || revision != 0 {
		t.Fatalf("failed document consumed revision %d: %v", revision, revisionErr)
	}
	result := execute(t, executor, "MATCH (n) RETURN count(n) AS total", nil)
	if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != int64(0) {
		t.Fatalf("failed document left nodes: %#v", got)
	}
}

func TestMutationInvalidMetadataAndPropertyTextHaveNoSideEffects(t *testing.T) {
	executor, database := testEngine(t)
	invalidUTF8 := string([]byte{0xff})
	for _, request := range []app.ExecuteRequest{
		{Query: "CREATE (:InvalidActor)", Actor: invalidUTF8},
		{Query: "CREATE (:InvalidProperty {value: $value})", Params: map[string]any{"value": invalidUTF8}},
	} {
		if _, err := executor.Execute(context.Background(), request); !errors.Is(err, store.ErrInvalidArgument) || !errors.Is(err, domain.ErrInvalidText) {
			t.Fatalf("invalid mutation error = %v", err)
		}
	}
	if revision, err := database.CurrentRevision(context.Background()); err != nil || revision != 0 {
		t.Fatalf("invalid mutations consumed revision %d: %v", revision, err)
	}
}
