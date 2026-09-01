// Package cypher parses sheets's documented Cypher subset into a
// lossless-enough, execution-neutral abstract syntax tree. It deliberately
// does not know about sheets storage: callers can inspect the tree, validate
// it further, or translate it to a plan.
package cypher

// Position identifies a byte position in the input. Lines and columns are
// one-based; Offset is zero-based.
type Position struct {
	Offset int
	Line   int
	Column int
}

// Span is the half-open source range occupied by a node.
type Span struct {
	Start Position
	End   Position
}

// Node is implemented by every AST value.
type Node interface {
	Location() Span
}

// Document is the result of parsing one source string. A document may contain
// more than one semicolon-separated statement. Empty statements are ignored.
// A non-nil error from Parse can still be accompanied by a partial document.
type Document struct {
	Source     string
	Statements []Statement
}

// StatementInfo contains execution-independent facts established by parsing.
// Mutates is conservative: a CALL statement is regarded as mutating because a
// procedure's write behaviour is not known without a procedure catalog.
type StatementInfo struct {
	Mutates  bool
	ReadOnly bool
}

// Statement is a top-level Cypher statement.
type Statement interface {
	Node
	statementNode()
	Info() StatementInfo
	IsMutation() bool
	IsReadOnly() bool
}

// QueryStatement is an ordered pipeline of Cypher clauses. Explain and
// Profile retain their modifiers so clients need not special-case them.
type QueryStatement struct {
	Span          Span
	Explain       bool
	Profile       bool
	Clauses       []Clause
	UnionBranches []UnionBranch
}

func (*QueryStatement) statementNode()   {}
func (s *QueryStatement) Location() Span { return s.Span }
func (s *QueryStatement) Info() StatementInfo {
	for _, clause := range s.Clauses {
		if clause.Mutates() {
			return StatementInfo{Mutates: true}
		}
	}
	for _, branch := range s.UnionBranches {
		if branch.Query != nil && branch.Query.IsMutation() {
			return StatementInfo{Mutates: true}
		}
	}
	return StatementInfo{ReadOnly: true}
}
func (s *QueryStatement) IsMutation() bool { return s.Info().Mutates }
func (s *QueryStatement) IsReadOnly() bool { return !s.IsMutation() }

// UnionBranch is the query following UNION or UNION ALL. A UNION query is
// represented by its first branch in QueryStatement.Clauses and every later
// branch here, in source order.
type UnionBranch struct {
	Span  Span
	All   bool
	Query *QueryStatement
}

// Clause is one stage in a query pipeline.
type Clause interface {
	Node
	clauseNode()
	Mutates() bool
}

// MatchClause finds paths. Optional distinguishes OPTIONAL MATCH from MATCH.
type MatchClause struct {
	Span     Span
	Optional bool
	Patterns []PatternPart
	Where    Expression
}

func (*MatchClause) clauseNode()      {}
func (c *MatchClause) Location() Span { return c.Span }
func (*MatchClause) Mutates() bool    { return false }

// UnwindClause turns a list expression into rows.
type UnwindClause struct {
	Span       Span
	Expression Expression
	Alias      Identifier
}

func (*UnwindClause) clauseNode()      {}
func (c *UnwindClause) Location() Span { return c.Span }
func (*UnwindClause) Mutates() bool    { return false }

// ProjectionClause represents WITH and RETURN. Where is valid only for WITH.
type ProjectionClause struct {
	Span     Span
	With     bool
	Distinct bool
	Items    []ProjectionItem
	Where    Expression
	OrderBy  []SortItem
	Skip     Expression
	Limit    Expression
}

func (*ProjectionClause) clauseNode()      {}
func (c *ProjectionClause) Location() Span { return c.Span }
func (*ProjectionClause) Mutates() bool    { return false }

// ProjectionItem projects an expression, or Star for a bare '*'. Alias is
// empty when no AS alias was supplied.
type ProjectionItem struct {
	Span       Span
	Star       bool
	Expression Expression
	Alias      Identifier
}

// SortItem is one ORDER BY item. Descending is false for ASC or omitted.
type SortItem struct {
	Span       Span
	Expression Expression
	Descending bool
}

// CreateClause creates every supplied pattern.
type CreateClause struct {
	Span     Span
	Patterns []PatternPart
}

func (*CreateClause) clauseNode()      {}
func (c *CreateClause) Location() Span { return c.Span }
func (*CreateClause) Mutates() bool    { return true }

// MergeClause matches or creates a single pattern, then optionally runs SET
// actions depending on which branch happened.
type MergeClause struct {
	Span    Span
	Pattern PatternPart
	Actions []MergeAction
}

func (*MergeClause) clauseNode()      {}
func (c *MergeClause) Location() Span { return c.Span }
func (*MergeClause) Mutates() bool    { return true }

// MergeActionKind describes the branch on which a MERGE action runs.
type MergeActionKind uint8

const (
	OnCreate MergeActionKind = iota + 1
	OnMatch
)

// MergeAction currently has the SET form supported by OpenCypher.
type MergeAction struct {
	Span Span
	Kind MergeActionKind
	Set  []SetItem
}

// SetClause assigns properties, whole property maps, or labels.
type SetClause struct {
	Span  Span
	Items []SetItem
}

func (*SetClause) clauseNode()      {}
func (c *SetClause) Location() Span { return c.Span }
func (*SetClause) Mutates() bool    { return true }

// SetItem is a SET operation. For LabelSet, Labels is non-empty and the other
// fields are zero. For assignments, Target and Value are set and Operator is
// either "=" or "+=".
type SetItem struct {
	Span     Span
	Target   Expression
	Operator string
	Value    Expression
	Labels   []Identifier
}

// RemoveClause removes properties or labels.
type RemoveClause struct {
	Span  Span
	Items []RemoveItem
}

func (*RemoveClause) clauseNode()      {}
func (c *RemoveClause) Location() Span { return c.Span }
func (*RemoveClause) Mutates() bool    { return true }

// RemoveItem removes either a property expression or labels from a variable.
type RemoveItem struct {
	Span   Span
	Target Expression
	Labels []Identifier
}

// DeleteClause deletes expressions. Detach requests relationship detachment.
type DeleteClause struct {
	Span        Span
	Detach      bool
	Expressions []Expression
}

func (*DeleteClause) clauseNode()      {}
func (c *DeleteClause) Location() Span { return c.Span }
func (*DeleteClause) Mutates() bool    { return true }

// CallClause invokes a procedure or a subquery. Procedure calls are
// conservatively classified as mutating; a future catalog may refine that.
type CallClause struct {
	Span       Span
	Procedure  QualifiedName
	Arguments  []Expression
	Subquery   *QueryStatement
	Yield      []YieldItem
	YieldWhere Expression
}

func (*CallClause) clauseNode()      {}
func (c *CallClause) Location() Span { return c.Span }
func (*CallClause) Mutates() bool    { return true }

// YieldItem exposes a procedure field, optionally under an alias. Star is
// true for YIELD *.
type YieldItem struct {
	Span  Span
	Star  bool
	Name  Identifier
	Alias Identifier
}

// Identifier preserves the decoded spelling of an identifier. BacktickQuoted
// tells consumers whether it was escaped in the source.
type Identifier struct {
	Span           Span
	Name           string
	BacktickQuoted bool
}

func (i Identifier) Location() Span { return i.Span }

// QualifiedName is a dot-separated procedure/function name.
type QualifiedName struct {
	Span  Span
	Parts []Identifier
}

func (n QualifiedName) Location() Span { return n.Span }
func (n QualifiedName) String() string {
	result := ""
	for i, part := range n.Parts {
		if i > 0 {
			result += "."
		}
		result += part.Name
	}
	return result
}

// PatternPart is an optionally named graph pattern (for example p = (a)-->(b)).
type PatternPart struct {
	Span     Span
	Variable Identifier
	Element  PatternElement
}

// PatternElement alternates Nodes and Relationships; it always has at least
// one node, and len(Nodes) is len(Relationships)+1.
type PatternElement struct {
	Span          Span
	Nodes         []NodePattern
	Relationships []RelationshipPattern
}

// NodePattern matches a graph node.
type NodePattern struct {
	Span       Span
	Variable   Identifier
	Labels     []Identifier
	Properties Expression // normally *MapLiteral, but kept general for parameters
}

// Direction is the direction drawn by a relationship pattern.
type Direction uint8

const (
	Undirected Direction = iota
	Outgoing
	Incoming
	// Bidirectional is the syntactically valid <--> form. Matching traverses it
	// in either direction, while mutation validation rejects it because
	// creation needs exactly one direction.
	Bidirectional
)

// RelationshipPattern matches a relationship adjacent to a pattern node.
type RelationshipPattern struct {
	Span       Span
	Variable   Identifier
	Types      []Identifier
	Length     *RelationshipLength
	Properties Expression
	Direction  Direction
}

// RelationshipLength represents * or *lower..upper. Nil bounds mean open.
type RelationshipLength struct {
	Span  Span
	Lower Expression
	Upper Expression
	// Exact distinguishes *3 from *3..3. It is false for * and ranges.
	Exact bool
}

// Expression is any Cypher expression.
type Expression interface {
	Node
	expressionNode()
}

// LiteralKind identifies literal syntax.
type LiteralKind uint8

const (
	NullLiteral LiteralKind = iota + 1
	BooleanLiteral
	IntegerLiteral
	FloatLiteral
	StringLiteral
)

// Literal holds its typed Go value (nil, bool, int64, float64, or string).
type Literal struct {
	Span  Span
	Kind  LiteralKind
	Value any
}

func (*Literal) expressionNode()  {}
func (e *Literal) Location() Span { return e.Span }

// Variable is an expression naming a value in the current row.
type Variable struct {
	Span Span
	Name Identifier
}

func (*Variable) expressionNode()  {}
func (e *Variable) Location() Span { return e.Span }

// Parameter references a value supplied by the host, such as $limit.
type Parameter struct {
	Span Span
	Name Identifier
}

func (*Parameter) expressionNode()  {}
func (e *Parameter) Location() Span { return e.Span }

// UnaryExpression applies Operator (NOT, +, or -) to Expression.
type UnaryExpression struct {
	Span       Span
	Operator   string
	Expression Expression
}

func (*UnaryExpression) expressionNode()  {}
func (e *UnaryExpression) Location() Span { return e.Span }

// BinaryExpression applies an infix operator to Left and Right.
type BinaryExpression struct {
	Span     Span
	Left     Expression
	Operator string
	Right    Expression
}

func (*BinaryExpression) expressionNode()  {}
func (e *BinaryExpression) Location() Span { return e.Span }

// IsNullExpression is the postfix IS [NOT] NULL predicate.
type IsNullExpression struct {
	Span       Span
	Expression Expression
	Not        bool
}

func (*IsNullExpression) expressionNode()  {}
func (e *IsNullExpression) Location() Span { return e.Span }

// PropertyExpression gets a named property from Expression.
type PropertyExpression struct {
	Span       Span
	Expression Expression
	Property   Identifier
}

func (*PropertyExpression) expressionNode()  {}
func (e *PropertyExpression) Location() Span { return e.Span }

// LabelExpression is the n:Label predicate used in WHERE expressions.
type LabelExpression struct {
	Span       Span
	Expression Expression
	Labels     []Identifier
}

func (*LabelExpression) expressionNode()  {}
func (e *LabelExpression) Location() Span { return e.Span }

// IndexExpression gets one list/map index.
type IndexExpression struct {
	Span       Span
	Expression Expression
	Index      Expression
}

func (*IndexExpression) expressionNode()  {}
func (e *IndexExpression) Location() Span { return e.Span }

// SliceExpression gets a list slice. Either bound may be nil.
type SliceExpression struct {
	Span       Span
	Expression Expression
	Start      Expression
	End        Expression
}

func (*SliceExpression) expressionNode()  {}
func (e *SliceExpression) Location() Span { return e.Span }

// FunctionInvocation invokes a qualified function name. Distinct applies to
// aggregate arguments (for example count(DISTINCT n)). Star represents count(*).
type FunctionInvocation struct {
	Span      Span
	Name      QualifiedName
	Distinct  bool
	Star      bool
	Arguments []Expression
}

func (*FunctionInvocation) expressionNode()  {}
func (e *FunctionInvocation) Location() Span { return e.Span }

// ListLiteral is an ordered expression list.
type ListLiteral struct {
	Span     Span
	Elements []Expression
}

func (*ListLiteral) expressionNode()  {}
func (e *ListLiteral) Location() Span { return e.Span }

// MapLiteral is a string-keyed expression map.
type MapLiteral struct {
	Span    Span
	Entries []MapEntry
}

func (*MapLiteral) expressionNode()  {}
func (e *MapLiteral) Location() Span { return e.Span }

// MapEntry is one map key and value.
type MapEntry struct {
	Span  Span
	Key   Identifier
	Value Expression
}

// CaseExpression represents simple and searched CASE forms.
type CaseExpression struct {
	Span         Span
	Operand      Expression // nil for searched CASE
	Alternatives []CaseAlternative
	Else         Expression
}

func (*CaseExpression) expressionNode()  {}
func (e *CaseExpression) Location() Span { return e.Span }

// CaseAlternative is one WHEN condition THEN value branch.
type CaseAlternative struct {
	Span Span
	When Expression
	Then Expression
}

// ListComprehension represents [x IN list WHERE predicate | projection].
type ListComprehension struct {
	Span       Span
	Variable   Identifier
	List       Expression
	Where      Expression
	Projection Expression
}

func (*ListComprehension) expressionNode()  {}
func (e *ListComprehension) Location() Span { return e.Span }

// PatternComprehension represents [path = (a)-->(b) WHERE predicate |
// projection]. Variable is empty when the path itself is not named.
type PatternComprehension struct {
	Span       Span
	Variable   Identifier
	Pattern    PatternElement
	Where      Expression
	Projection Expression
}

func (*PatternComprehension) expressionNode()  {}
func (e *PatternComprehension) Location() Span { return e.Span }

// ListPredicate represents all/any/none/single(variable IN list WHERE test).
type ListPredicate struct {
	Span     Span
	Operator string
	Variable Identifier
	List     Expression
	Where    Expression
}

func (*ListPredicate) expressionNode()  {}
func (e *ListPredicate) Location() Span { return e.Span }

// ReduceExpression represents reduce(accumulator = initial, variable IN list |
// expression).
type ReduceExpression struct {
	Span        Span
	Accumulator Identifier
	Initial     Expression
	Variable    Identifier
	List        Expression
	Expression  Expression
}

func (*ReduceExpression) expressionNode()  {}
func (e *ReduceExpression) Location() Span { return e.Span }

// PatternExpression uses a graph pattern as an expression (for example in
// exists((a)-->(b)) or shortestPath((a)-[*]->(b))).
type PatternExpression struct {
	Span    Span
	Pattern PatternElement
}

func (*PatternExpression) expressionNode()  {}
func (e *PatternExpression) Location() Span { return e.Span }

// ExistsSubquery is EXISTS { ... }.
type ExistsSubquery struct {
	Span     Span
	Subquery *QueryStatement
}

func (*ExistsSubquery) expressionNode()  {}
func (e *ExistsSubquery) Location() Span { return e.Span }
