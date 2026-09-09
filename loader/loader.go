// Package loader adds configuration-driven assembly on top of the cordis core.
//
// It is the Go equivalent of Cordis's loader and include plugins: a tree of
// entries names plugins, carries their config, and can be layered with patches.
// Because Go cannot import code at runtime, plugins register themselves in a
// Registry at compile time and the configuration only selects and configures
// them - the same trade-off Caddy makes.
package loader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/tr1v3r/cordis-go"
)

// Patch is one raw configuration entry.
//
// Pointer fields distinguish "absent" from "explicitly set": a patch only
// overrides the fields it mentions, while Config replaces the target config as a
// whole (Cordis never deep-merges config).
type Patch struct {
	ID       string         `json:"id,omitempty"`
	Name     *string        `json:"name,omitempty"`
	Label    *string        `json:"label,omitempty"`
	Disabled *bool          `json:"disabled,omitempty"`
	Group    *bool          `json:"group,omitempty"`
	Inject   *[]string      `json:"inject,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
	Plugins  []*Patch       `json:"plugins,omitempty"`
	Insert   []*Patch       `json:"insert,omitempty"`
}

// Layer is one ordered configuration file.
type Layer struct {
	// Label identifies the layer in provenance output.
	Label string
	// Entries are the file's entries, in order.
	Entries []*Patch
	// Patch marks a patch layer. A patch layer's entries must match an existing
	// id (or carry "insert") and only override the fields they mention; a base
	// layer creates entries instead.
	Patch bool
}

// ParseLayer decodes a JSON array of entries as a base layer.
func ParseLayer(label string, data []byte) (Layer, error) {
	entries, err := parseEntries(label, data)
	if err != nil {
		return Layer{}, err
	}
	return Layer{Label: label, Entries: entries}, nil
}

// ParsePatchLayer decodes a JSON array of entries as a patch layer.
func ParsePatchLayer(label string, data []byte) (Layer, error) {
	entries, err := parseEntries(label, data)
	if err != nil {
		return Layer{}, err
	}
	return Layer{Label: label, Entries: entries, Patch: true}, nil
}

func parseEntries(label string, data []byte) ([]*Patch, error) {
	var entries []*Patch
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("layer %s: %w", label, err)
	}
	return entries, nil
}

// LoadLayer reads and parses a JSON configuration file as a base layer.
func LoadLayer(label, path string) (Layer, error) {
	return loadLayerFile(label, path, false)
}

// LoadPatchLayer reads and parses a JSON configuration file as a patch layer.
func LoadPatchLayer(label, path string) (Layer, error) {
	return loadLayerFile(label, path, true)
}

func loadLayerFile(label, path string, patch bool) (Layer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Layer{}, fmt.Errorf("layer %s: %w", label, err)
	}
	if label == "" {
		label = path
	}
	if patch {
		return ParsePatchLayer(label, data)
	}
	return ParseLayer(label, data)
}

// Node is one composed configuration entry.
type Node struct {
	ID       string
	Name     string
	Label    string
	Disabled bool
	Group    bool
	Inject   []string
	Config   map[string]any
	Children []*Node

	// Source is the layer that created this node.
	Source string
	// Patched lists the layers that changed it afterwards, in order.
	Patched []string
}

// Tree is the result of composing layers.
type Tree struct {
	// Layers lists the layer labels in application order.
	Layers []string
	// Nodes is the composed entry tree.
	Nodes []*Node
	// Warnings records non-fatal composition problems, such as a patch that
	// matched no entry. This is deliberate: Cordis's include plugin silently
	// drops such patches, which is a recurring source of "why is my config not
	// applied" bugs.
	Warnings []string
}

// ComposeOption customizes Compose.
type ComposeOption func(*composeOptions)

type composeOptions struct {
	strict bool
}

// Strict turns an unmatched patch id into an error instead of a warning.
func Strict() ComposeOption {
	return func(o *composeOptions) { o.strict = true }
}

// Compose applies layers in order and returns the resulting tree.
//
// Within a layer, an entry whose id matches an existing node patches it; an
// entry carrying "insert" appends new nodes. Ids are matched across the whole
// tree, so a nested group child can be patched by a later layer.
func Compose(layers []Layer, opts ...ComposeOption) (*Tree, error) {
	options := &composeOptions{}
	for _, opt := range opts {
		opt(options)
	}

	tree := &Tree{}
	index := map[string]*Node{}
	for _, layer := range layers {
		tree.Layers = append(tree.Layers, layer.Label)
		for _, entry := range layer.Entries {
			var err error
			if layer.Patch {
				err = applyPatch(tree, index, layer.Label, &tree.Nodes, entry, options)
			} else {
				err = applyBase(tree, index, layer.Label, &tree.Nodes, entry)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return tree, nil
}

// applyBase creates entries for a base layer.
func applyBase(tree *Tree, index map[string]*Node, source string, siblings *[]*Node, entry *Patch) error {
	if entry == nil {
		return nil
	}
	items := entry.Insert
	if len(items) == 0 {
		items = []*Patch{entry}
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		if item.ID == "" && item.Name == nil {
			return fmt.Errorf("layer %s: entry requires id or name", source)
		}
		if item.ID != "" {
			if _, exists := index[item.ID]; exists {
				return fmt.Errorf("layer %s: duplicate entry id %q", source, item.ID)
			}
		}
		node := createNode(item, source)
		*siblings = append(*siblings, node)
		indexNode(index, node)
	}
	return nil
}

func applyPatch(tree *Tree, index map[string]*Node, source string, siblings *[]*Node, patch *Patch, options *composeOptions) error {
	if patch == nil {
		return nil
	}
	if len(patch.Insert) > 0 {
		for _, item := range patch.Insert {
			if item == nil {
				continue
			}
			if item.ID == "" && item.Name == nil {
				return fmt.Errorf("layer %s: inserted entry requires id or name", source)
			}
			if item.ID != "" {
				if _, exists := index[item.ID]; exists {
					message := fmt.Sprintf("layer %s: duplicate entry id %q", source, item.ID)
					if options.strict {
						return fmt.Errorf("%s", message)
					}
					tree.Warnings = append(tree.Warnings, message)
					continue
				}
			}
			node := createNode(item, source)
			*siblings = append(*siblings, node)
			indexNode(index, node)
		}
		return nil
	}
	if patch.ID == "" {
		return fmt.Errorf("layer %s: entry requires id or insert", source)
	}
	node, ok := index[patch.ID]
	if !ok {
		message := fmt.Sprintf("layer %s: patch id %q matched no entry", source, patch.ID)
		if options.strict {
			return fmt.Errorf("%s", message)
		}
		tree.Warnings = append(tree.Warnings, message)
		return nil
	}
	mergePatch(node, patch, source)
	for _, child := range patch.Plugins {
		if err := applyPatch(tree, index, source, &node.Children, child, options); err != nil {
			return err
		}
	}
	return nil
}

func createNode(patch *Patch, source string) *Node {
	node := &Node{ID: patch.ID, Source: source}
	if patch.Name != nil {
		node.Name = *patch.Name
	}
	if patch.Label != nil {
		node.Label = *patch.Label
	}
	if patch.Disabled != nil {
		node.Disabled = *patch.Disabled
	}
	if patch.Group != nil {
		node.Group = *patch.Group
	}
	if patch.Inject != nil {
		node.Inject = append([]string(nil), (*patch.Inject)...)
	}
	if patch.Config != nil {
		node.Config = cloneMap(patch.Config)
	}
	for _, child := range patch.Plugins {
		if child == nil {
			continue
		}
		node.Children = append(node.Children, createNode(child, source))
	}
	return node
}

func mergePatch(node *Node, patch *Patch, source string) {
	if patch.Name != nil {
		node.Name = *patch.Name
	}
	if patch.Label != nil {
		node.Label = *patch.Label
	}
	if patch.Disabled != nil {
		node.Disabled = *patch.Disabled
	}
	if patch.Group != nil {
		node.Group = *patch.Group
	}
	if patch.Inject != nil {
		node.Inject = append([]string(nil), (*patch.Inject)...)
	}
	if patch.Config != nil {
		// Whole-config replacement, matching Cordis: a patch never deep-merges.
		node.Config = cloneMap(patch.Config)
	}
	node.Patched = append(node.Patched, source)
}

// indexNode registers a node and all of its descendants in the global id
// index, so a later layer can patch a nested group child by id.
func indexNode(index map[string]*Node, node *Node) {
	if node == nil {
		return
	}
	if node.ID != "" {
		index[node.ID] = node
	}
	for _, child := range node.Children {
		indexNode(index, child)
	}
}

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// Find returns the node with the given id, searching the whole tree.
func (t *Tree) Find(id string) *Node {
	var walk func([]*Node) *Node
	walk = func(nodes []*Node) *Node {
		for _, node := range nodes {
			if node.ID == id {
				return node
			}
			if found := walk(node.Children); found != nil {
				return found
			}
		}
		return nil
	}
	return walk(t.Nodes)
}

// Size reports how many entries the tree holds.
func (t *Tree) Size() int {
	var count func([]*Node) int
	count = func(nodes []*Node) int {
		total := len(nodes)
		for _, node := range nodes {
			total += count(node.Children)
		}
		return total
	}
	return count(t.Nodes)
}

// Registry maps configuration entry names to plugin definitions.
type Registry struct {
	mu    sync.RWMutex
	items map[string]*registered
}

type registered struct {
	name   string
	inject []string
	load   func(ctx *cordis.Context, config map[string]any, extra []string) (*cordis.Fiber, error)
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{items: map[string]*registered{}}
}

// Register adds a plugin under the name used by configuration entries.
func Register[C any](registry *Registry, name string, plugin *cordis.Plugin[C]) error {
	if registry == nil {
		return fmt.Errorf("loader: nil registry")
	}
	if name == "" {
		return fmt.Errorf("loader: plugin name must not be empty")
	}
	if plugin == nil {
		return fmt.Errorf("loader: plugin %q is nil", name)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.items[name]; exists {
		return fmt.Errorf("loader: plugin %q is already registered", name)
	}
	registry.items[name] = &registered{
		name:   name,
		inject: append([]string(nil), plugin.Inject...),
		load: func(ctx *cordis.Context, config map[string]any, extra []string) (*cordis.Fiber, error) {
			typed, err := decodeConfig[C](config)
			if err != nil {
				return nil, err
			}
			return cordis.LoadWithInject(ctx, plugin, typed, extra...)
		},
	}
	return nil
}

// MustRegister is Register with a panic on failure, for package init blocks.
func MustRegister[C any](registry *Registry, name string, plugin *cordis.Plugin[C]) {
	if err := Register(registry, name, plugin); err != nil {
		panic(err)
	}
}

func (r *Registry) get(name string) (*registered, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.items[name]
	return entry, ok
}

// Has reports whether a plugin name is registered.
func (r *Registry) Has(name string) bool {
	_, ok := r.get(name)
	return ok
}

// Names returns the registered plugin names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.items))
	for name := range r.items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func decodeConfig[C any](raw map[string]any) (C, error) {
	var config C
	if len(raw) == 0 {
		return config, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return config, fmt.Errorf("loader: encode config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("loader: decode config %s: %w", string(data), err)
	}
	return config, nil
}

// Load instantiates every enabled entry of the tree in ctx.
//
// Group entries create a child context scope rather than a plugin, and disabled
// entries are skipped. The returned fibers are owned by ctx and are disposed
// with it.
func (t *Tree) Load(ctx *cordis.Context, registry *Registry) ([]*cordis.Fiber, error) {
	if ctx == nil {
		return nil, fmt.Errorf("loader: nil context")
	}
	if registry == nil {
		return nil, fmt.Errorf("loader: nil registry")
	}
	var fibers []*cordis.Fiber
	if err := t.loadNodes(ctx, registry, t.Nodes, nil, &fibers); err != nil {
		// Configuration loading is all-or-nothing: a half-applied tree is
		// harder to reason about than no tree at all.
		for i := len(fibers) - 1; i >= 0; i-- {
			fibers[i].Dispose()
		}
		return nil, err
	}
	return fibers, nil
}

func (t *Tree) loadNodes(ctx *cordis.Context, registry *Registry, nodes []*Node, inherited []string, fibers *[]*cordis.Fiber) error {
	for _, node := range nodes {
		if node == nil || node.Disabled {
			continue
		}
		// A group's inject gates its whole subtree, so it is threaded into every
		// descendant entry instead of being dropped.
		deps := append(append([]string(nil), inherited...), node.Inject...)
		if node.Group {
			label := node.Label
			if label == "" {
				label = node.ID
			}
			if label == "" {
				label = "group"
			}
			if err := t.loadNodes(ctx.Fork(label), registry, node.Children, deps, fibers); err != nil {
				return err
			}
			continue
		}
		entry, ok := registry.get(node.Name)
		if !ok {
			return fmt.Errorf("loader: entry %q references unknown plugin %q", node.ID, node.Name)
		}
		fiber, err := entry.load(ctx, node.Config, deps)
		if err != nil {
			return fmt.Errorf("loader: entry %q (%s): %w", node.ID, node.Name, err)
		}
		if fiber.State() == cordis.StateFailed {
			// A failed plugin body is a configuration error too: surface it so
			// the caller sees all-or-nothing instead of a half-loaded tree.
			return fmt.Errorf("loader: entry %q (%s): %w", node.ID, node.Name, fiber.Error())
		}
		*fibers = append(*fibers, fiber)
	}
	return nil
}
