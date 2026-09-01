package tui

import (
	"bytes"
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
	"github.com/svlocks/sheets/internal/engine"
	"github.com/svlocks/sheets/internal/store"
)

type engineBackend struct {
	root   string
	engine *engine.Engine
}

func (b engineBackend) ProjectRoot() string { return b.root }

func (b engineBackend) Execute(ctx context.Context, request app.ExecuteRequest) (app.BatchResult, error) {
	return b.engine.Execute(ctx, request)
}

func (b engineBackend) CurrentRevision(ctx context.Context) (domain.Revision, error) {
	return b.engine.CurrentRevision(ctx)
}

func (b engineBackend) Revisions(ctx context.Context) ([]domain.RevisionInfo, error) {
	var revisions []domain.RevisionInfo
	page := domain.Page{Limit: 100}
	for {
		values, info, err := b.engine.ListRevisions(ctx, page)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, values...)
		if info.Next == "" {
			return revisions, nil
		}
		page.After = info.Next
	}
}

func TestGeneratedMutationFlowsExecuteAgainstRealEngine(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "sheets.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	executor, err := engine.New(database)
	if err != nil {
		t.Fatalf("engine.New() error = %v", err)
	}
	backend := engineBackend{root: t.TempDir(), engine: executor}

	createRoot := newCreateNodeForm(1, newGraphState(nil, nil), "", 80, 24, true, true)
	rootData := createRoot.data.(*nodeFormData)
	rootData.title = "Root"
	rootData.labels = "Task"
	rootRequest, err := createRoot.request()
	if err != nil {
		t.Fatalf("root form request error = %v", err)
	}
	if _, err := executor.Execute(ctx, rootRequest); err != nil {
		t.Fatalf("execute root request: %v\n%s", err, rootRequest.Query)
	}

	loaded, err := loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load root snapshot: %v", err)
	}
	rootID := loaded.graph.firstNodeID()
	createChild := newCreateNodeForm(2, loaded.graph, rootID, 80, 24, true, true)
	childData := createChild.data.(*nodeFormData)
	childData.title = "Child"
	childData.labels = "Task, Feature"
	childData.position = "0"
	childRequest, err := createChild.request()
	if err != nil {
		t.Fatalf("child form request error = %v", err)
	}
	childResult, err := executor.Execute(ctx, childRequest)
	if err != nil {
		t.Fatalf("execute child request: %v\n%s", err, childRequest.Query)
	}
	if childResult.Revision == nil {
		t.Fatal("child creation did not return a revision")
	}
	childRevision := *childResult.Revision

	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load child snapshot: %v", err)
	}
	var childNode domain.Node
	for _, node := range loaded.graph.nodes {
		if nodeTitle(node) == "Child" {
			childNode = node
		}
	}
	if childNode.ID == "" {
		t.Fatal("created child is missing")
	}

	createDestination := newCreateNodeForm(3, loaded.graph, "", 80, 24, true, true)
	destinationData := createDestination.data.(*nodeFormData)
	destinationData.title = "Destination"
	destinationData.labels = "Task"
	destinationRequest, err := createDestination.request()
	if err != nil {
		t.Fatalf("destination form request error = %v", err)
	}
	if _, err := executor.Execute(ctx, destinationRequest); err != nil {
		t.Fatalf("execute destination request: %v\n%s", err, destinationRequest.Query)
	}

	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load destination snapshot: %v", err)
	}
	var destinationID domain.EntityID
	for _, node := range loaded.graph.nodes {
		if nodeTitle(node) == "Destination" {
			destinationID = node.ID
		}
	}
	if destinationID == "" {
		t.Fatal("created destination is missing")
	}

	editNode := newEditNodeForm(4, loaded.graph.nodeByID[childNode.ID], 80, 24, true, true)
	editNodeData := editNode.data.(*nodeFormData)
	editNodeData.title = "Edited child"
	editNodeData.labels = "Task, Reviewed"
	editNodeData.properties = `{"priority":2}`
	editNodeData.body = "# Edited\n\nReady to move."
	editNodeRequest, err := editNode.request()
	if err != nil {
		t.Fatalf("edit node form request error = %v", err)
	}
	if _, err := executor.Execute(ctx, editNodeRequest); err != nil {
		t.Fatalf("execute node edit: %v\n%s", err, editNodeRequest.Query)
	}

	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load edited node snapshot: %v", err)
	}
	childNode = loaded.graph.nodeByID[childNode.ID]
	if nodeTitle(childNode) != "Edited child" || childNode.Body != editNodeData.body || childNode.Properties["priority"] != int64(2) {
		t.Fatalf("edited child = %#v", childNode)
	}

	move := newMoveNodeForm(5, loaded.graph, childNode, 80, 24, true, true)
	moveData := move.data.(*moveFormData)
	moveData.parent = string(destinationID)
	moveData.position = ""
	moveRequest, err := move.request()
	if err != nil {
		t.Fatalf("move form request error = %v", err)
	}
	if _, err := executor.Execute(ctx, moveRequest); err != nil {
		t.Fatalf("execute move request: %v\n%s", err, moveRequest.Query)
	}

	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load moved snapshot: %v", err)
	}
	parent, ok := loaded.graph.parent[childNode.ID]
	if !ok || parent.From != destinationID || parent.Position != nil {
		t.Fatalf("moved parent = %#v", parent)
	}
	connect := newConnectionForm(6, loaded.graph, rootID, 80, 24, true, true)
	connectionData := connect.data.(*connectionFormData)
	connectionData.to = string(childNode.ID)
	connectionData.typeName = "BLOCKED_BY"
	connectionData.properties = `{"reason":"test"}`
	connectRequest, err := connect.request()
	if err != nil {
		t.Fatalf("connect form request error = %v", err)
	}
	if _, err := executor.Execute(ctx, connectRequest); err != nil {
		t.Fatalf("execute connect request: %v\n%s", err, connectRequest.Query)
	}

	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load relationship snapshot: %v", err)
	}
	var relationship domain.Edge
	for _, edge := range loaded.graph.edges {
		if edge.Type == "BLOCKED_BY" {
			relationship = edge
		}
	}
	if relationship.ID == "" {
		t.Fatal("created relationship is missing")
	}
	edit := newEditRelationshipForm(7, relationship, 80, 24, true, true)
	edit.data.(*relationshipFormData).properties = `{"reason":"updated","weight":2,"body":"edge body"}`
	editRequest, err := edit.request()
	if err != nil {
		t.Fatalf("edit relationship request error = %v", err)
	}
	if _, err := executor.Execute(ctx, editRequest); err != nil {
		t.Fatalf("execute relationship edit: %v\n%s", err, editRequest.Query)
	}
	editedSnapshot, err := loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load edited relationship snapshot: %v", err)
	}
	editedRelationship := editedSnapshot.graph.edgeByID[relationship.ID]
	if editedRelationship.Properties["body"] != "edge body" || editedRelationship.Properties["weight"] != int64(2) {
		t.Fatalf("edited relationship properties = %#v", editedRelationship.Properties)
	}

	deleteForm := newDeleteRelationshipForm(8, loaded.graph, relationship, 80, 24, true, true)
	deleteForm.data.(*confirmFormData).confirmed = true
	deleteRequest, err := deleteForm.request()
	if err != nil {
		t.Fatalf("delete relationship request error = %v", err)
	}
	if _, err := executor.Execute(ctx, deleteRequest); err != nil {
		t.Fatalf("execute relationship delete: %v\n%s", err, deleteRequest.Query)
	}

	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load relationship-deleted snapshot: %v", err)
	}
	deleteNode := newDeleteNodeForm(9, loaded.graph, loaded.graph.nodeByID[childNode.ID], 80, 24, true, true)
	deleteNode.data.(*confirmFormData).confirmed = true
	deleteNodeRequest, err := deleteNode.request()
	if err != nil {
		t.Fatalf("delete node form request error = %v", err)
	}
	if _, err := executor.Execute(ctx, deleteNodeRequest); err != nil {
		t.Fatalf("execute node delete: %v\n%s", err, deleteNodeRequest.Query)
	}
	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load node-deleted snapshot: %v", err)
	}
	if _, exists := loaded.graph.nodeByID[childNode.ID]; exists {
		t.Fatal("deleted node remains in the live graph")
	}
	historical, err := loadSnapshot(ctx, backend, domain.Snapshot{Revision: &childRevision})
	if err != nil {
		t.Fatalf("load historical child snapshot: %v", err)
	}
	if _, exists := historical.graph.nodeByID[childNode.ID]; !exists {
		t.Fatal("deleted node is missing from its historical revision")
	}
	parent, exists := historical.graph.parent[childNode.ID]
	if !exists || parent.From != rootID || parent.Position == nil || *parent.Position != 0 {
		t.Fatalf("historical CHILD edge = %#v", parent)
	}
}

func TestStaleReparentCannotCommitAnUnintendedDetach(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "sheets.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	executor, err := engine.New(database)
	if err != nil {
		t.Fatalf("engine.New() error = %v", err)
	}
	backend := engineBackend{root: t.TempDir(), engine: executor}

	_, err = executor.Execute(ctx, app.ExecuteRequest{
		Query:   "CREATE (old:Task {title: 'Old'})-[:CHILD {position: 1}]->(child:Task {title: 'Child'}), (destination:Task {title: 'Destination'})",
		Actor:   "test",
		Message: "seed move race",
	})
	if err != nil {
		t.Fatalf("seed graph: %v", err)
	}
	loaded, err := loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load seeded snapshot: %v", err)
	}
	var childNode domain.Node
	var destinationID domain.EntityID
	for _, node := range loaded.graph.nodes {
		switch nodeTitle(node) {
		case "Child":
			childNode = node
		case "Destination":
			destinationID = node.ID
		}
	}
	oldParent := loaded.graph.parent[childNode.ID]
	move := newMoveNodeForm(1, loaded.graph, childNode, 80, 24, true, true)
	moveData := move.data.(*moveFormData)
	moveData.parent = string(destinationID)
	moveData.position = ""
	moveRequest, err := move.request()
	if err != nil {
		t.Fatalf("move request: %v", err)
	}

	if _, err := executor.Execute(ctx, app.ExecuteRequest{
		Query:  "MATCH (n) WHERE elementId(n) = $id DETACH DELETE n",
		Params: map[string]any{"id": string(destinationID)}, Actor: "other process", Message: "remove destination",
	}); err != nil {
		t.Fatalf("delete stale destination: %v", err)
	}
	moveResult, err := executor.Execute(ctx, moveRequest)
	if err != nil {
		t.Fatalf("stale move execution should be a no-match, got %v\n%s", err, moveRequest.Query)
	}
	if moveResult.Revision != nil || batchChanged(moveResult) || batchHasRows(moveResult) {
		t.Fatalf("stale move unexpectedly changed or matched the graph: %#v", moveResult)
	}
	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load after stale move: %v", err)
	}
	parent, exists := loaded.graph.parent[childNode.ID]
	if !exists || parent.ID != oldParent.ID || parent.From != oldParent.From || parent.Position == nil || *parent.Position != 1 {
		t.Fatalf("stale move detached or rewrote the original CHILD edge: %#v", parent)
	}

	// Changing order under the same parent updates the existing relationship,
	// while submitting the already-current value is a matched no-op.
	reorder := newMoveNodeForm(2, loaded.graph, loaded.graph.nodeByID[childNode.ID], 80, 24, true, true)
	reorder.data.(*moveFormData).position = ""
	reorderRequest, err := reorder.request()
	if err != nil {
		t.Fatalf("reorder request: %v", err)
	}
	if _, err := executor.Execute(ctx, reorderRequest); err != nil {
		t.Fatalf("execute reorder: %v\n%s", err, reorderRequest.Query)
	}
	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load reordered snapshot: %v", err)
	}
	parent = loaded.graph.parent[childNode.ID]
	if parent.ID != oldParent.ID || parent.Position != nil {
		t.Fatalf("reorder recreated the edge or retained its position: %#v", parent)
	}
	noOp := newMoveNodeForm(3, loaded.graph, loaded.graph.nodeByID[childNode.ID], 80, 24, true, true)
	noOpRequest, err := noOp.request()
	if err != nil {
		t.Fatalf("no-op move request: %v", err)
	}
	noOpResult, err := executor.Execute(ctx, noOpRequest)
	if err != nil {
		t.Fatalf("execute no-op move: %v", err)
	}
	if noOpResult.Revision != nil || !batchHasRows(noOpResult) {
		t.Fatalf("matched no-op move result = %#v", noOpResult)
	}
}

func TestTypedPropertiesSurviveARealFormEdit(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "sheets.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	executor, err := engine.New(database)
	if err != nil {
		t.Fatalf("engine.New() error = %v", err)
	}
	backend := engineBackend{root: t.TempDir(), engine: executor}
	zone := time.FixedZone("Audit/Fixed", -5*60*60)
	when := time.Date(2026, time.August, 31, 12, 34, 56, 123, zone)
	date, _ := temporal.ParseDate("1984-10-11")
	localTime, _ := temporal.ParseLocalTime("12:31:14.645876123")
	offsetTime, _ := temporal.ParseTime("12:31:14.645876123+01:00")
	localDateTime := temporal.NewLocalDateTime(date, localTime)
	dateTime, _ := temporal.NewDateTime(localDateTime, "Europe/Stockholm")
	cypherDuration, _ := temporal.NewDuration(-7, 14, -4, 500_000_000)
	properties := domain.Properties{
		"title": "Typed", "float": float64(1), "nan": math.NaN(), "when": when,
		"duration": 90 * time.Minute, "bytes": []byte{0, 1, 2, 255},
		"date": date, "local_time": localTime, "offset_time": offsetTime,
		"local_datetime": localDateTime, "zoned_datetime": dateTime,
		"cypher_duration": cypherDuration,
		"nested":          map[string]any{"values": []any{math.Inf(-1), when, dateTime, cypherDuration}},
	}
	if _, err := executor.Execute(ctx, app.ExecuteRequest{
		Query: "CREATE (n:Task) SET n = $properties RETURN n", Params: map[string]any{"properties": properties},
		Actor: "test", Message: "seed typed properties",
	}); err != nil {
		t.Fatalf("seed typed properties: %v", err)
	}
	loaded, err := loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("load typed node: %v", err)
	}
	node := loaded.graph.nodes[0]
	form := newEditNodeForm(1, node, 80, 24, true, true)
	form.data.(*nodeFormData).title = "Typed edited"
	request, err := form.request()
	if err != nil {
		t.Fatalf("typed edit request: %v", err)
	}
	if _, err := executor.Execute(ctx, request); err != nil {
		t.Fatalf("execute typed edit: %v\n%s", err, request.Query)
	}
	loaded, err = loadSnapshot(ctx, backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("reload typed node: %v", err)
	}
	got := loaded.graph.nodes[0].Properties
	if got["title"] != "Typed edited" {
		t.Fatalf("edited title = %#v", got["title"])
	}
	if value, ok := got["float"].(float64); !ok || value != 1 {
		t.Fatalf("finite float changed = %#v (%T)", got["float"], got["float"])
	}
	if value, ok := got["nan"].(float64); !ok || !math.IsNaN(value) {
		t.Fatalf("NaN changed = %#v", got["nan"])
	}
	if value, ok := got["when"].(time.Time); !ok || !value.Equal(when) || value.Location().String() != when.Location().String() {
		t.Fatalf("temporal changed = %#v", got["when"])
	}
	if value, ok := got["duration"].(time.Duration); !ok || value != 90*time.Minute {
		t.Fatalf("duration changed = %#v (%T)", got["duration"], got["duration"])
	}
	if value, ok := got["bytes"].([]byte); !ok || !bytes.Equal(value, []byte{0, 1, 2, 255}) {
		t.Fatalf("bytes changed = %#v (%T)", got["bytes"], got["bytes"])
	}
	if value, ok := got["date"].(temporal.Date); !ok || !value.Equal(date) {
		t.Fatalf("date changed = %#v", got["date"])
	}
	if value, ok := got["local_time"].(temporal.LocalTime); !ok || !value.Equal(localTime) {
		t.Fatalf("local time changed = %#v", got["local_time"])
	}
	if value, ok := got["offset_time"].(temporal.Time); !ok || !value.Equal(offsetTime) {
		t.Fatalf("offset time changed = %#v", got["offset_time"])
	}
	if value, ok := got["local_datetime"].(temporal.LocalDateTime); !ok || !value.Equal(localDateTime) {
		t.Fatalf("local date-time changed = %#v", got["local_datetime"])
	}
	if value, ok := got["zoned_datetime"].(temporal.DateTime); !ok || !value.Equal(dateTime) {
		t.Fatalf("zoned date-time changed = %#v", got["zoned_datetime"])
	}
	if value, ok := got["cypher_duration"].(temporal.Duration); !ok || !value.Equal(cypherDuration) {
		t.Fatalf("Cypher duration changed = %#v", got["cypher_duration"])
	}
	nested := got["nested"].(domain.Properties)["values"].([]any)
	if value, ok := nested[0].(float64); !ok || !math.IsInf(value, -1) {
		t.Fatalf("nested infinity changed = %#v", nested[0])
	}
	if value, ok := nested[1].(time.Time); !ok || !value.Equal(when) || value.Location().String() != when.Location().String() {
		t.Fatalf("nested temporal changed = %#v", nested[1])
	}
	if value, ok := nested[2].(temporal.DateTime); !ok || !value.Equal(dateTime) {
		t.Fatalf("nested zoned date-time changed = %#v", nested[2])
	}
	if value, ok := nested[3].(temporal.Duration); !ok || !value.Equal(cypherDuration) {
		t.Fatalf("nested Cypher duration changed = %#v", nested[3])
	}
}
