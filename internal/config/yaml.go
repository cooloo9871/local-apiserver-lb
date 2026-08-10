package config

import (
	"fmt"
	"os"
	"strings"
)

// yamlDoc is the result of parsing the restricted YAML subset: a flat
// mapping of scalar values plus string lists.
type yamlDoc struct {
	scalars map[string]string
	lists   map[string][]string
}

// parseYAMLSubset parses a deliberately restricted YAML subset:
//
//   - flat "key: value" scalar pairs at the top level
//   - string lists, either flow style ("key: [a, b]") or block style
//     ("- item" lines indented under a bare "key:")
//   - "#" comments (full-line and trailing)
//   - optional single or double quotes around values
//
// Nested mappings, anchors, multi-line strings, and tab indentation are
// rejected. This keeps the parser small, dependency-free, and testable;
// the config schema is flat so nothing more is needed.
func parseYAMLSubset(data []byte) (*yamlDoc, error) {
	doc := &yamlDoc{
		scalars: make(map[string]string),
		lists:   make(map[string][]string),
	}

	var pendingList string // key awaiting block-list items, "" if none

	for i, raw := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		line := strings.TrimRight(raw, " \r")
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.ContainsAny(line[:len(line)-len(trimmed)], "\t") {
			return nil, fmt.Errorf("line %d: tab indentation is not supported", lineNo)
		}

		indented := len(trimmed) < len(line)

		if indented {
			if pendingList == "" {
				return nil, fmt.Errorf("line %d: unexpected indented line", lineNo)
			}
			item, ok := strings.CutPrefix(trimmed, "- ")
			if !ok {
				return nil, fmt.Errorf("line %d: expected list item under %q; nested mappings are not supported", lineNo, pendingList)
			}
			val, err := parseScalar(item, lineNo)
			if err != nil {
				return nil, err
			}
			doc.lists[pendingList] = append(doc.lists[pendingList], val)
			continue
		}

		// A new top-level key terminates any pending block list.
		if pendingList != "" && len(doc.lists[pendingList]) == 0 {
			return nil, fmt.Errorf("key %q has no value", pendingList)
		}
		pendingList = ""

		key, rest, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", lineNo)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", lineNo)
		}
		if _, dup := doc.scalars[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", lineNo, key)
		}
		if _, dup := doc.lists[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", lineNo, key)
		}

		rest = strings.TrimSpace(rest)
		switch {
		case rest == "" || strings.HasPrefix(rest, "#"):
			pendingList = key
			doc.lists[key] = nil
		case strings.HasPrefix(rest, "["):
			items, err := parseFlowList(rest, lineNo)
			if err != nil {
				return nil, err
			}
			doc.lists[key] = items
		default:
			val, err := parseScalar(rest, lineNo)
			if err != nil {
				return nil, err
			}
			doc.scalars[key] = val
		}
	}

	if pendingList != "" && len(doc.lists[pendingList]) == 0 {
		return nil, fmt.Errorf("key %q has no value", pendingList)
	}
	return doc, nil
}

// parseScalar strips a trailing comment and optional quotes from a value.
func parseScalar(s string, lineNo int) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > 0 && (s[0] == '"' || s[0] == '\'') {
		quote := s[0]
		end := strings.IndexByte(s[1:], quote)
		if end < 0 {
			return "", fmt.Errorf("line %d: unterminated quoted string", lineNo)
		}
		val := s[1 : 1+end]
		rest := strings.TrimSpace(s[2+end:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("line %d: unexpected trailing content %q", lineNo, rest)
		}
		return val, nil
	}
	if idx := strings.Index(s, " #"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s, nil
}

// parseFlowList parses a "[a, b, c]" flow-style list.
func parseFlowList(s string, lineNo int) ([]string, error) {
	if idx := strings.LastIndexByte(s, ']'); idx < 0 {
		return nil, fmt.Errorf("line %d: unterminated flow list", lineNo)
	} else {
		rest := strings.TrimSpace(s[idx+1:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return nil, fmt.Errorf("line %d: unexpected trailing content %q", lineNo, rest)
		}
		s = s[1:idx]
	}
	var items []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		val, err := parseScalar(part, lineNo)
		if err != nil {
			return nil, err
		}
		items = append(items, val)
	}
	return items, nil
}

// applyConfigFile merges values from the config file into cfg. Flags the
// user set explicitly on the command line take precedence; everything else
// in the file overrides built-in defaults. fileSet applies a named value
// through the flag set so that type parsing and error messages match the
// command line behavior.
func (c *Config) applyConfigFile(path string, fileSet func(name, value string) error, known func(name string) bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	doc, err := parseYAMLSubset(data)
	if err != nil {
		return fmt.Errorf("parsing config file %s: %w", path, err)
	}

	for key := range doc.lists {
		if key != "servers" {
			return fmt.Errorf("config file %s: key %q must be a scalar", path, key)
		}
	}

	for key, val := range doc.scalars {
		switch {
		case key == "config" || key == "version":
			return fmt.Errorf("config file %s: key %q is not allowed in a config file", path, key)
		case key == "servers":
			if !c.Explicit("servers") {
				c.Servers = splitServers(val)
			}
		case !known(key):
			return fmt.Errorf("config file %s: unknown key %q", path, key)
		case !c.Explicit(key):
			if err := fileSet(key, val); err != nil {
				return fmt.Errorf("config file %s: key %q: %w", path, key, err)
			}
		}
	}

	if list, ok := doc.lists["servers"]; ok && !c.Explicit("servers") {
		c.Servers = list
	}
	return nil
}

// LoadServersFromFile re-reads only the server list from a config file.
// It is used for SIGHUP reloads, where all other settings stay fixed.
func LoadServersFromFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	doc, err := parseYAMLSubset(data)
	if err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	servers := doc.lists["servers"]
	if csv, ok := doc.scalars["servers"]; ok {
		servers = splitServers(csv)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("config file %s: no servers defined", path)
	}
	return servers, nil
}
