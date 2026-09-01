package main

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strings"
)

type tckStep struct {
	Text  string
	Doc   string
	Table [][]string
	Line  int
}

type scenarioDefinition struct {
	ID       string
	Name     string
	Outline  bool
	Tags     []string
	Steps    []tckStep
	Examples [][]string
	Line     int
}

type scenarioInstance struct {
	ID           string
	DefinitionID string
	Name         string
	Tags         []string
	Steps        []tckStep
	Example      int
}

type featureDocument struct {
	Definitions []scenarioDefinition
	Instances   []scenarioInstance
}

func parseFeature(relative string, reader io.Reader) (featureDocument, error) {
	lines := make([]string, 0, 256)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return featureDocument{}, err
	}

	relative = path.Clean(relative)
	var (
		document    featureDocument
		background  []tckStep
		pendingTags []string
	)
	for index := 0; index < len(lines); {
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "@") {
			pendingTags = append(pendingTags, strings.Fields(trimmed)...)
			index++
			continue
		}
		if trimmed == "Background:" {
			steps, next, err := parseSteps(lines, index+1, false)
			if err != nil {
				return featureDocument{}, fmt.Errorf("line %d: %w", index+1, err)
			}
			background = steps
			index = next
			continue
		}
		outline := strings.HasPrefix(trimmed, "Scenario Outline:")
		if !outline && !strings.HasPrefix(trimmed, "Scenario:") {
			index++
			continue
		}

		name := strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
		definition := scenarioDefinition{
			ID:      relative + "::" + name,
			Name:    name,
			Outline: outline,
			Tags:    append([]string(nil), pendingTags...),
			Line:    index + 1,
		}
		pendingTags = nil
		steps, next, err := parseSteps(lines, index+1, outline)
		if err != nil {
			return featureDocument{}, fmt.Errorf("%s line %d: %w", definition.ID, index+1, err)
		}
		definition.Steps = append(append([]tckStep(nil), background...), steps...)
		index = next
		if outline {
			for index < len(lines) && strings.TrimSpace(lines[index]) != "Examples:" {
				candidate := strings.TrimSpace(lines[index])
				if strings.HasPrefix(candidate, "Scenario:") || strings.HasPrefix(candidate, "Scenario Outline:") {
					return featureDocument{}, fmt.Errorf("%s has no Examples table", definition.ID)
				}
				index++
			}
			if index == len(lines) {
				return featureDocument{}, fmt.Errorf("%s has no Examples table", definition.ID)
			}
			index++
			for index < len(lines) {
				candidate := strings.TrimSpace(lines[index])
				if strings.HasPrefix(candidate, "Scenario:") || strings.HasPrefix(candidate, "Scenario Outline:") ||
					candidate == "Background:" || strings.HasPrefix(candidate, "@") {
					break
				}
				if strings.HasPrefix(candidate, "|") {
					row, tableErr := parseTableRow(candidate)
					if tableErr != nil {
						return featureDocument{}, fmt.Errorf("%s Examples line %d: %w", definition.ID, index+1, tableErr)
					}
					definition.Examples = append(definition.Examples, row)
				}
				index++
			}
			if len(definition.Examples) < 2 {
				return featureDocument{}, fmt.Errorf("%s Examples table has no data rows", definition.ID)
			}
		}
		document.Definitions = append(document.Definitions, definition)
	}

	seen := make(map[string]struct{})
	for _, definition := range document.Definitions {
		instances, err := instantiateScenario(definition)
		if err != nil {
			return featureDocument{}, err
		}
		for _, instance := range instances {
			if _, duplicate := seen[instance.ID]; duplicate {
				return featureDocument{}, fmt.Errorf("duplicate scenario ID %q", instance.ID)
			}
			seen[instance.ID] = struct{}{}
			document.Instances = append(document.Instances, instance)
		}
	}
	return document, nil
}

func parseSteps(lines []string, start int, stopAtExamples bool) ([]tckStep, int, error) {
	steps := make([]tckStep, 0, 8)
	index := start
	for index < len(lines) {
		trimmed := strings.TrimSpace(lines[index])
		if strings.HasPrefix(trimmed, "Scenario:") || strings.HasPrefix(trimmed, "Scenario Outline:") ||
			trimmed == "Background:" || strings.HasPrefix(trimmed, "@") {
			break
		}
		if stopAtExamples && trimmed == "Examples:" {
			break
		}
		text, ok := stepText(trimmed)
		if !ok {
			index++
			continue
		}
		step := tckStep{Text: text, Line: index + 1}
		index++
		for index < len(lines) && strings.TrimSpace(lines[index]) == "" {
			index++
		}
		if index < len(lines) && strings.TrimSpace(lines[index]) == `"""` {
			var err error
			step.Doc, index, err = parseDocString(lines, index)
			if err != nil {
				return nil, index, err
			}
		}
		for index < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index]), "|") {
			row, err := parseTableRow(strings.TrimSpace(lines[index]))
			if err != nil {
				return nil, index, fmt.Errorf("line %d: %w", index+1, err)
			}
			step.Table = append(step.Table, row)
			index++
		}
		steps = append(steps, step)
	}
	return steps, index, nil
}

func stepText(line string) (string, bool) {
	for _, keyword := range []string{"Given ", "When ", "Then ", "And ", "But "} {
		if strings.HasPrefix(line, keyword) {
			return strings.TrimSpace(strings.TrimPrefix(line, keyword)), true
		}
	}
	return "", false
}

func parseDocString(lines []string, opener int) (string, int, error) {
	contents := make([]string, 0, 8)
	for index := opener + 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == `"""` {
			return dedent(contents), index + 1, nil
		}
		contents = append(contents, lines[index])
	}
	return "", len(lines), fmt.Errorf("unterminated doc string at line %d", opener+1)
}

func dedent(lines []string) string {
	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		width := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent == -1 || width < indent {
			indent = width
		}
	}
	if indent < 0 {
		return ""
	}
	for index, line := range lines {
		if len(line) >= indent {
			lines[index] = line[indent:]
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func parseTableRow(line string) ([]string, error) {
	if line == "|" {
		return []string{}, nil
	}
	if len(line) < 2 || line[0] != '|' || line[len(line)-1] != '|' {
		return nil, fmt.Errorf("malformed table row %q", line)
	}
	var (
		cells   []string
		current strings.Builder
		escaped bool
	)
	for _, character := range line[1 : len(line)-1] {
		if escaped {
			switch character {
			case 'n':
				current.WriteByte('\n')
			case 'r':
				current.WriteByte('\r')
			case '|', '\\':
				current.WriteRune(character)
			default:
				current.WriteByte('\\')
				current.WriteRune(character)
			}
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '|' {
			cells = append(cells, strings.TrimSpace(current.String()))
			current.Reset()
			continue
		}
		current.WriteRune(character)
	}
	if escaped {
		current.WriteByte('\\')
	}
	cells = append(cells, strings.TrimSpace(current.String()))
	return cells, nil
}

func instantiateScenario(definition scenarioDefinition) ([]scenarioInstance, error) {
	if !definition.Outline {
		return []scenarioInstance{{
			ID: definition.ID, DefinitionID: definition.ID, Name: definition.Name,
			Tags: append([]string(nil), definition.Tags...), Steps: cloneSteps(definition.Steps),
		}}, nil
	}
	headings := definition.Examples[0]
	instances := make([]scenarioInstance, 0, len(definition.Examples)-1)
	for rowIndex, row := range definition.Examples[1:] {
		if len(row) != len(headings) {
			return nil, fmt.Errorf("%s example %d has %d cells, want %d", definition.ID, rowIndex+1, len(row), len(headings))
		}
		values := make(map[string]string, len(headings))
		for index, heading := range headings {
			values[heading] = row[index]
		}
		instance := scenarioInstance{
			ID:           fmt.Sprintf("%s::example[%03d]", definition.ID, rowIndex+1),
			DefinitionID: definition.ID,
			Name:         substituteExamples(definition.Name, values),
			Tags:         append([]string(nil), definition.Tags...),
			Steps:        cloneSteps(definition.Steps),
			Example:      rowIndex + 1,
		}
		for stepIndex := range instance.Steps {
			step := &instance.Steps[stepIndex]
			step.Text = substituteExamples(step.Text, values)
			step.Doc = substituteExamples(step.Doc, values)
			for tableRow := range step.Table {
				for cell := range step.Table[tableRow] {
					step.Table[tableRow][cell] = substituteExamples(step.Table[tableRow][cell], values)
				}
			}
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func cloneSteps(source []tckStep) []tckStep {
	result := make([]tckStep, len(source))
	for index, step := range source {
		result[index] = step
		result[index].Table = make([][]string, len(step.Table))
		for row := range step.Table {
			result[index].Table[row] = append([]string(nil), step.Table[row]...)
		}
	}
	return result
}

func substituteExamples(source string, values map[string]string) string {
	var result strings.Builder
	for len(source) > 0 {
		open := strings.IndexByte(source, '<')
		if open < 0 {
			result.WriteString(source)
			break
		}
		result.WriteString(source[:open])
		source = source[open:]
		close := strings.IndexByte(source, '>')
		if close < 0 {
			result.WriteString(source)
			break
		}
		name := source[1:close]
		value, exists := values[name]
		if !exists {
			// A literal less-than operator can occur immediately before an
			// Examples placeholder ("< <rhs>"). Consuming through the next
			// greater-than here would hide that real placeholder. Preserve only
			// the unmatched '<' and resume scanning after it.
			result.WriteByte('<')
			source = source[1:]
			continue
		}
		result.WriteString(value)
		source = source[close+1:]
	}
	return result.String()
}
