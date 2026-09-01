package cypher

import (
	"strings"

	"github.com/svlocks/sheets/internal/cypher/parsergen"
)

func (b *cstBinder) bindPattern(ctx parsergen.IOC_PatternContext) ([]PatternPart, error) {
	patterns := make([]PatternPart, 0, len(ctx.AllOC_PatternPart()))
	for _, partCtx := range ctx.AllOC_PatternPart() {
		part, err := b.bindPatternPart(partCtx)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, part)
	}
	return patterns, nil
}

func (b *cstBinder) bindPatternPart(ctx parsergen.IOC_PatternPartContext) (PatternPart, error) {
	part := PatternPart{Span: b.span(ctx)}
	if variable := ctx.OC_Variable(); variable != nil {
		part.Variable = b.bindVariableIdentifier(variable)
	}
	element, err := b.bindPatternElement(ctx.OC_AnonymousPatternPart().OC_PatternElement())
	part.Element = element
	return part, err
}

func (b *cstBinder) bindPatternElement(ctx parsergen.IOC_PatternElementContext) (PatternElement, error) {
	if nested := ctx.OC_PatternElement(); nested != nil {
		element, err := b.bindPatternElement(nested)
		element.Span = b.span(ctx)
		return element, err
	}
	first, err := b.bindNodePattern(ctx.OC_NodePattern())
	if err != nil {
		return PatternElement{}, err
	}
	element := PatternElement{Span: b.span(ctx), Nodes: []NodePattern{first}}
	for _, chain := range ctx.AllOC_PatternElementChain() {
		relationship, relationshipErr := b.bindRelationshipPattern(chain.OC_RelationshipPattern())
		if relationshipErr != nil {
			return PatternElement{}, relationshipErr
		}
		node, nodeErr := b.bindNodePattern(chain.OC_NodePattern())
		if nodeErr != nil {
			return PatternElement{}, nodeErr
		}
		element.Relationships = append(element.Relationships, relationship)
		element.Nodes = append(element.Nodes, node)
	}
	return element, nil
}

func (b *cstBinder) bindRelationshipsPattern(ctx parsergen.IOC_RelationshipsPatternContext) (PatternElement, error) {
	first, err := b.bindNodePattern(ctx.OC_NodePattern())
	if err != nil {
		return PatternElement{}, err
	}
	element := PatternElement{Span: b.span(ctx), Nodes: []NodePattern{first}}
	for _, chain := range ctx.AllOC_PatternElementChain() {
		relationship, relationshipErr := b.bindRelationshipPattern(chain.OC_RelationshipPattern())
		if relationshipErr != nil {
			return PatternElement{}, relationshipErr
		}
		node, nodeErr := b.bindNodePattern(chain.OC_NodePattern())
		if nodeErr != nil {
			return PatternElement{}, nodeErr
		}
		element.Relationships = append(element.Relationships, relationship)
		element.Nodes = append(element.Nodes, node)
	}
	return element, nil
}

func (b *cstBinder) bindNodePattern(ctx parsergen.IOC_NodePatternContext) (NodePattern, error) {
	node := NodePattern{Span: b.span(ctx)}
	if variable := ctx.OC_Variable(); variable != nil {
		node.Variable = b.bindVariableIdentifier(variable)
	}
	if labels := ctx.OC_NodeLabels(); labels != nil {
		node.Labels = b.bindNodeLabels(labels)
	}
	if properties := ctx.OC_Properties(); properties != nil {
		var err error
		node.Properties, err = b.bindProperties(properties)
		if err != nil {
			return NodePattern{}, err
		}
	}
	return node, nil
}

func (b *cstBinder) bindNodeLabels(ctx parsergen.IOC_NodeLabelsContext) []Identifier {
	if ctx == nil {
		return nil
	}
	labels := make([]Identifier, 0, len(ctx.AllOC_NodeLabel()))
	for _, label := range ctx.AllOC_NodeLabel() {
		labels = append(labels, b.bindIdentifier(label.OC_LabelName().OC_SchemaName()))
	}
	return labels
}

func (b *cstBinder) bindRelationshipPattern(ctx parsergen.IOC_RelationshipPatternContext) (RelationshipPattern, error) {
	relationship := RelationshipPattern{Span: b.span(ctx), Direction: Undirected}
	if ctx.OC_LeftArrowHead() != nil && ctx.OC_RightArrowHead() != nil {
		relationship.Direction = Bidirectional
	} else if ctx.OC_LeftArrowHead() != nil {
		relationship.Direction = Incoming
	} else if ctx.OC_RightArrowHead() != nil {
		relationship.Direction = Outgoing
	}
	detail := ctx.OC_RelationshipDetail()
	if detail == nil {
		return relationship, nil
	}
	if variable := detail.OC_Variable(); variable != nil {
		relationship.Variable = b.bindVariableIdentifier(variable)
	}
	if types := detail.OC_RelationshipTypes(); types != nil {
		for _, typeCtx := range types.AllOC_RelTypeName() {
			relationship.Types = append(relationship.Types, b.bindIdentifier(typeCtx.OC_SchemaName()))
		}
	}
	if rangeCtx := detail.OC_RangeLiteral(); rangeCtx != nil {
		length, err := b.bindRelationshipLength(rangeCtx)
		if err != nil {
			return RelationshipPattern{}, err
		}
		relationship.Length = length
	}
	if properties := detail.OC_Properties(); properties != nil {
		var err error
		relationship.Properties, err = b.bindProperties(properties)
		if err != nil {
			return RelationshipPattern{}, err
		}
	}
	return relationship, nil
}

func (b *cstBinder) bindRelationshipLength(ctx parsergen.IOC_RangeLiteralContext) (*RelationshipLength, error) {
	length := &RelationshipLength{Span: b.span(ctx)}
	integers := ctx.AllOC_IntegerLiteral()
	hasRange := strings.Contains(ctx.GetText(), "..")
	if !hasRange && len(integers) == 1 {
		literal, err := b.bindIntegerLiteral(integers[0], false, b.span(integers[0]))
		if err != nil {
			return nil, err
		}
		length.Lower = literal
		length.Upper = literal
		length.Exact = true
		return length, nil
	}
	if !hasRange {
		return length, nil
	}
	rangeToken := directToken(ctx, "..")
	rangeStart := -1
	if rangeToken != nil {
		rangeStart = rangeToken.GetSymbol().GetStart()
	}
	for _, integer := range integers {
		literal, err := b.bindIntegerLiteral(integer, false, b.span(integer))
		if err != nil {
			return nil, err
		}
		if integer.GetStart().GetStart() < rangeStart {
			length.Lower = literal
		} else {
			length.Upper = literal
		}
	}
	return length, nil
}

func (b *cstBinder) bindProperties(ctx parsergen.IOC_PropertiesContext) (Expression, error) {
	if mapCtx := ctx.OC_MapLiteral(); mapCtx != nil {
		return b.bindMapLiteral(mapCtx)
	}
	if parameter := ctx.OC_Parameter(); parameter != nil {
		return b.bindParameter(parameter), nil
	}
	return nil, b.unsupported(ctx, "pattern properties", "expected a map literal or parameter")
}
