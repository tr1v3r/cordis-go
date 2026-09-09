package loader

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Dump renders the composed tree with provenance comments.
//
// It mirrors `dsh --profile <name> --dump-config`: the output is meant to answer
// "what will actually be loaded, and which layer decided that?", which the
// source layers alone cannot answer once patches replace whole configs.
func (t *Tree) Dump(w io.Writer) error {
	var builder strings.Builder
	builder.WriteString("# cordis-go config dump\n")
	builder.WriteString("# layers: " + strings.Join(t.Layers, " -> ") + "\n")
	for _, warning := range t.Warnings {
		builder.WriteString("# warning: " + warning + "\n")
	}
	if len(t.Nodes) == 0 {
		builder.WriteString("# (empty)\n")
	}
	for _, node := range t.Nodes {
		dumpNode(&builder, node, "")
	}
	_, err := io.WriteString(w, builder.String())
	return err
}

// DumpString returns the dump as a string.
func (t *Tree) DumpString() string {
	var builder strings.Builder
	_ = t.Dump(&builder)
	return builder.String()
}

func dumpNode(builder *strings.Builder, node *Node, indent string) {
	provenance := node.Source
	if len(node.Patched) > 0 {
		provenance += "; patched by " + strings.Join(node.Patched, ", ")
	}
	if provenance == "" {
		provenance = "unknown"
	}

	if node.ID != "" {
		fmt.Fprintf(builder, "%s- id: %s", indent, quote(node.ID))
	} else {
		// An entry may be declared by name only; never print a bare empty id.
		fmt.Fprintf(builder, "%s- name: %s", indent, quote(node.Name))
	}
	if node.Group {
		builder.WriteString("  # group")
	}
	builder.WriteString("  # from " + provenance + "\n")
	if node.Name != "" && node.ID != "" {
		fmt.Fprintf(builder, "%s  name: %s\n", indent, quote(node.Name))
	}
	if node.Label != "" {
		fmt.Fprintf(builder, "%s  label: %s\n", indent, quote(node.Label))
	}
	if node.Disabled {
		fmt.Fprintf(builder, "%s  disabled: true\n", indent)
	}
	if len(node.Inject) > 0 {
		fmt.Fprintf(builder, "%s  inject: %s\n", indent, quoteList(node.Inject))
	}
	if len(node.Config) > 0 {
		fmt.Fprintf(builder, "%s  config: %s\n", indent, dumpConfig(node.Config))
	}
	if len(node.Children) > 0 {
		fmt.Fprintf(builder, "%s  plugins:\n", indent)
		for _, child := range node.Children {
			dumpNode(builder, child, indent+"    ")
		}
	}
}

// dumpConfig renders a config map with sorted keys so dumps are stable.
func dumpConfig(config map[string]any) string {
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(quote(key))
		builder.WriteString(": ")
		builder.WriteString(dumpValue(config[key]))
	}
	builder.WriteByte('}')
	return builder.String()
}

func dumpValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return dumpConfig(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, dumpValue(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return string(data)
	}
}

func quote(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		return `"` + value + `"`
	}
	return string(data)
}

func quoteList(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, quote(value))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
