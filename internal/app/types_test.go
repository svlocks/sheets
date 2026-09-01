package app

import (
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/domain"
)

func TestExecuteRequestValidate(t *testing.T) {
	revision := domain.Revision(1)
	now := time.Now()
	tests := []struct {
		name    string
		request ExecuteRequest
		wantErr bool
	}{
		{name: "valid", request: ExecuteRequest{Query: "RETURN 1"}},
		{name: "empty", request: ExecuteRequest{}, wantErr: true},
		{name: "whitespace", request: ExecuteRequest{Query: " \n\t"}, wantErr: true},
		{
			name: "two snapshots",
			request: ExecuteRequest{
				Query:    "RETURN 1",
				Snapshot: domain.Snapshot{Revision: &revision, Time: &now},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.request.Validate(); (got != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", got, tt.wantErr)
			}
		})
	}
}

func TestSummaryChanged(t *testing.T) {
	if (Summary{}).Changed() {
		t.Fatal("zero summary should not report a change")
	}
	if !(Summary{NodesCreated: 1}).Changed() {
		t.Fatal("created node should report a change")
	}
	if !(Summary{NodesCreated: ^uint64(0), NodesUpdated: 1}).Changed() {
		t.Fatal("counter overflow must not hide a change")
	}
}
