package kubeconfig

import (
	"fmt"
	"strings"
)

// This file implements a small tree parser for the restricted YAML subset
// found in machine-generated kubeconfig files (kubeadm, kubelet, service
// account kubeconfigs): indentation-based nested mappings, sequences of
// mappings ("- key: value"), plain or quoted scalars, "#" comments, and
// simple single-line flow collections ("{}", "{k: v}", "[a, b]").
// Anchors, aliases, multi-line scalars, tags, and multi-document streams
// are not supported. Values are map[string]any, []any, or string.

type yamlLine struct {
	indent  int
	content string // content with indentation stripped
	lineNo  int
}

// lexYAML splits input into meaningful lines, dropping blanks and
// full-line comments, and rejecting tab indentation.
func lexYAML(data []byte) ([]yamlLine, error) {
	var out []yamlLine
	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimRight(raw, " \r")
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(trimmed)
		if strings.ContainsRune(line[:indent], '\t') {
			return nil, fmt.Errorf("line %d: tab indentation is not supported", lineNo)
		}
		if trimmed == "---" {
			if len(out) > 0 {
				return nil, fmt.Errorf("line %d: multi-document YAML is not supported", lineNo)
			}
			continue
		}
		out = append(out, yamlLine{indent: indent, content: trimmed, lineNo: lineNo})
	}
	return out, nil
}

// parseYAMLTree parses the whole document into a tree.
func parseYAMLTree(data []byte) (any, error) {
	lines, err := lexYAML(data)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	p := &yamlParser{lines: lines}
	node, err := p.parseBlock(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.lines) {
		return nil, fmt.Errorf("line %d: unexpected content (indentation mismatch?)", p.lines[p.pos].lineNo)
	}
	return node, nil
}

type yamlParser struct {
	lines []yamlLine
	pos   int
}

// parseBlock parses a mapping or sequence whose entries sit at exactly
// the given indent.
func (p *yamlParser) parseBlock(indent int) (any, error) {
	if strings.HasPrefix(p.lines[p.pos].content, "- ") || p.lines[p.pos].content == "-" {
		return p.parseSequence(indent)
	}
	return p.parseMapping(indent)
}

func (p *yamlParser) parseSequence(indent int) (any, error) {
	var seq []any
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent != indent || (!strings.HasPrefix(line.content, "- ") && line.content != "-") {
			break
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(line.content, "-"), " ")
		itemIndent := indent + 2

		if rest == "" {
			// "-" alone: item is the following deeper block.
			p.pos++
			if p.pos >= len(p.lines) || p.lines[p.pos].indent < itemIndent {
				return nil, fmt.Errorf("line %d: empty sequence item", line.lineNo)
			}
			item, err := p.parseBlock(p.lines[p.pos].indent)
			if err != nil {
				return nil, err
			}
			seq = append(seq, item)
			continue
		}

		if !strings.Contains(rest, ":") || isFlowOrQuoted(rest) {
			// Plain scalar item.
			val, err := parseScalarValue(rest, line.lineNo)
			if err != nil {
				return nil, err
			}
			seq = append(seq, val)
			p.pos++
			continue
		}

		// "- key: ..." starts a mapping; treat the rest as a virtual
		// line at the item indent and let parseMapping consume the
		// following lines at that indent.
		p.lines[p.pos] = yamlLine{indent: itemIndent, content: rest, lineNo: line.lineNo}
		item, err := p.parseMapping(itemIndent)
		if err != nil {
			return nil, err
		}
		seq = append(seq, item)
	}
	return seq, nil
}

func (p *yamlParser) parseMapping(indent int) (any, error) {
	m := make(map[string]any)
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if line.indent != indent || strings.HasPrefix(line.content, "- ") || line.content == "-" {
			break
		}
		key, rest, err := splitKeyValue(line.content, line.lineNo)
		if err != nil {
			return nil, err
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", line.lineNo, key)
		}
		p.pos++

		if rest != "" {
			val, err := parseScalarValue(rest, line.lineNo)
			if err != nil {
				return nil, err
			}
			m[key] = val
			continue
		}

		// Bare "key:": value is the following deeper block, or null.
		if p.pos < len(p.lines) && (p.lines[p.pos].indent > indent ||
			(p.lines[p.pos].indent == indent && (strings.HasPrefix(p.lines[p.pos].content, "- ") || p.lines[p.pos].content == "-"))) {
			child, err := p.parseBlock(p.lines[p.pos].indent)
			if err != nil {
				return nil, err
			}
			m[key] = child
		} else {
			m[key] = ""
		}
	}
	return m, nil
}

// splitKeyValue splits "key: value" (or "key:") respecting quoted keys.
func splitKeyValue(s string, lineNo int) (key, rest string, err error) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("line %d: expected \"key: value\"", lineNo)
	}
	if idx+1 < len(s) && s[idx+1] != ' ' {
		// "key:value" without space is not a mapping separator in YAML
		// unless followed by end of line; scalars like URLs land here.
		return "", "", fmt.Errorf("line %d: expected space after \":\" in %q", lineNo, s)
	}
	key = strings.TrimSpace(s[:idx])
	key = unquote(key)
	if key == "" {
		return "", "", fmt.Errorf("line %d: empty key", lineNo)
	}
	return key, strings.TrimSpace(s[idx+1:]), nil
}

// isFlowOrQuoted reports whether a sequence item body is a quoted string
// or flow collection rather than an inline mapping.
func isFlowOrQuoted(s string) bool {
	return s[0] == '"' || s[0] == '\'' || s[0] == '[' || s[0] == '{'
}

// parseScalarValue interprets an inline value: quoted or plain scalar,
// or a single-line flow collection.
func parseScalarValue(s string, lineNo int) (any, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "{"):
		return parseFlowMap(s, lineNo)
	case strings.HasPrefix(s, "["):
		return parseFlowSeq(s, lineNo)
	case strings.HasPrefix(s, "&") || strings.HasPrefix(s, "*") || strings.HasPrefix(s, "|") || strings.HasPrefix(s, ">"):
		return nil, fmt.Errorf("line %d: YAML anchors, aliases, and block scalars are not supported", lineNo)
	}
	if s[0] == '"' || s[0] == '\'' {
		quote := s[0]
		end := strings.IndexByte(s[1:], quote)
		if end < 0 {
			return nil, fmt.Errorf("line %d: unterminated quoted string", lineNo)
		}
		val := s[1 : 1+end]
		rest := strings.TrimSpace(s[2+end:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return nil, fmt.Errorf("line %d: unexpected trailing content %q", lineNo, rest)
		}
		return val, nil
	}
	if idx := strings.Index(s, " #"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s, nil
}

// parseFlowMap parses "{k: v, k2: v2}" with scalar values only.
func parseFlowMap(s string, lineNo int) (any, error) {
	inner, err := flowInner(s, '{', '}', lineNo)
	if err != nil {
		return nil, err
	}
	m := make(map[string]any)
	for _, part := range splitFlowParts(inner) {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		key, rest, err := splitKeyValue(part, lineNo)
		if err != nil {
			return nil, err
		}
		val, err := parseScalarValue(rest, lineNo)
		if err != nil {
			return nil, err
		}
		m[key] = val
	}
	return m, nil
}

// parseFlowSeq parses "[a, b, c]" with scalar values only.
func parseFlowSeq(s string, lineNo int) (any, error) {
	inner, err := flowInner(s, '[', ']', lineNo)
	if err != nil {
		return nil, err
	}
	var seq []any
	for _, part := range splitFlowParts(inner) {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		val, err := parseScalarValue(part, lineNo)
		if err != nil {
			return nil, err
		}
		seq = append(seq, val)
	}
	return seq, nil
}

func flowInner(s string, open, close byte, lineNo int) (string, error) {
	idx := strings.LastIndexByte(s, close)
	if s[0] != open || idx < 0 {
		return "", fmt.Errorf("line %d: unterminated flow collection", lineNo)
	}
	rest := strings.TrimSpace(s[idx+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("line %d: unexpected trailing content %q", lineNo, rest)
	}
	return s[1:idx], nil
}

// splitFlowParts splits on commas outside quotes. Nested flow
// collections are not supported (kubeconfigs do not use them).
func splitFlowParts(s string) []string {
	var parts []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == ',':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
