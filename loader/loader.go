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
	"io"
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
//
// Numbers are decoded as json.Number rather than float64, so an integer beyond
// 2^53 keeps its exact value on the way into a plugin config and into Dump.
func ParseLayer(label string, data []byte) (Layer, error) {
	entries, err := parseEntries(label, data)
	if err != nil {
		return Layer{}, err
	}
	return Layer{Label: label, Entries: entries}, nil
}

// ParsePatchLayer decodes a JSON array of entries as a patch layer.
//
// Numbers are decoded as json.Number exactly as ParseLayer does.
func ParsePatchLayer(label string, data []byte) (Layer, error) {
	entries, err := parseEntries(label, data)
	if err != nil {
		return Layer{}, err
	}
	return Layer{Label: label, Entries: entries, Patch: true}, nil
}

// parseEntries decodes a layer file.
//
// It asks the decoder for json.Number instead of float64: the default decode
// rounds every integer beyond 2^53, so a config a file spells 9007199254740993
// would reach the plugin as 9007199254740992 with nothing to signal the loss.
// The values stay as text until decodeConfig hands them to the target type,
// which restores the usual encoding/json shapes for interface fields.
func parseEntries(label string, data []byte) ([]*Patch, error) {
	var entries []*Patch
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&entries); err != nil {
		return nil, fmt.Errorf("layer %s: %w", label, err)
	}
	// Decode consumes one value and stops, where json.Unmarshal rejected
	// leftovers. A layer file is exactly one array, so keep rejecting anything
	// after it instead of silently reading a prefix.
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("layer %s: unexpected data after the top-level value", label)
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
	if label == "" {
		label = path
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Layer{}, fmt.Errorf("layer %s: %w", label, err)
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

type treeComposer struct {
	tree    *Tree
	index   map[string]*Node
	options composeOptions
}

// Compose applies layers in order and returns the resulting tree.
//
// Within a layer, an entry whose id matches an existing node patches it; an
// entry carrying "insert" appends new nodes. Ids are matched across the whole
// tree, so a nested group child can be patched by a later layer.
func Compose(layers []Layer, opts ...ComposeOption) (*Tree, error) {
	composer := &treeComposer{
		tree:  &Tree{},
		index: map[string]*Node{},
	}
	for _, opt := range opts {
		opt(&composer.options)
	}

	for _, layer := range layers {
		if err := validateLayer(layer); err != nil {
			return nil, err
		}
		composer.tree.Layers = append(composer.tree.Layers, layer.Label)
		for _, entry := range layer.Entries {
			var err error
			if layer.Patch {
				err = composer.applyPatch(layer.Label, &composer.tree.Nodes, entry)
			} else {
				err = composer.applyBase(layer.Label, &composer.tree.Nodes, entry)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	// A non-group entry's children are never loaded, but they still take part in
	// the composed tree: they are counted, dumped and indexed. Say so after the
	// whole tree exists, because a later layer may turn the entry into a group.
	if err := composer.checkNonGroupChildren(); err != nil {
		return nil, err
	}
	return composer.tree, nil
}

// checkNonGroupChildren reports every entry that carries children without being
// a group: Load only recurses into groups, so such children are dropped without
// a trace unless Compose speaks up.
func (c *treeComposer) checkNonGroupChildren() error {
	var walk func(nodes []*Node) error
	walk = func(nodes []*Node) error {
		for _, node := range nodes {
			if node == nil {
				continue
			}
			if len(node.Children) > 0 && !node.Group {
				message := fmt.Sprintf(
					"entry %q has plugins but is not a group: they will not be loaded",
					nodeLabel(node))
				if c.options.strict {
					return fmt.Errorf("layer %s: %s", node.Source, message)
				}
				c.tree.Warnings = append(c.tree.Warnings, message)
			}
			if err := walk(node.Children); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(c.tree.Nodes)
}

// nodeLabel names a node in a diagnostic: its id, else its name, else a
// placeholder, because an entry may carry neither.
func nodeLabel(node *Node) string {
	if node.ID != "" {
		return node.ID
	}
	if node.Name != "" {
		return node.Name
	}
	return "<unnamed>"
}

// validateLayer rejects entries that carry "insert" together with fields of a
// normal entry, in whatever layer and at whatever depth they appear.
//
// Neither applyBase nor applyPatch can honour both halves of such an entry: the
// base path drops the outer entry, the patch path drops everything but the
// insert - including the "patch id matched no entry" warning the author would
// need to notice it. Failing loudly is the only way to keep half a config from
// disappearing.
func validateLayer(layer Layer) error {
	for _, entry := range layer.Entries {
		if err := validateEntry(layer.Label, entry); err != nil {
			return err
		}
	}
	return nil
}

func validateEntry(source string, entry *Patch) error {
	if entry == nil {
		return nil
	}
	if len(entry.Insert) > 0 && declaresEntryFields(entry) {
		return fmt.Errorf(
			"layer %s: entry %q declares both insert and other fields; split it into two entries",
			source, entryLabel(entry))
	}
	for _, child := range entry.Plugins {
		if err := validateEntry(source, child); err != nil {
			return err
		}
	}
	for _, inserted := range entry.Insert {
		if err := validateEntry(source, inserted); err != nil {
			return err
		}
	}
	return nil
}

// declaresEntryFields reports whether an entry carries anything that only makes
// sense without "insert".
func declaresEntryFields(entry *Patch) bool {
	return entry.ID != "" || entry.Name != nil || entry.Label != nil ||
		entry.Disabled != nil || entry.Group != nil || entry.Inject != nil ||
		entry.Config != nil || len(entry.Plugins) > 0
}

// entryLabel names an entry in a diagnostic: its id, else its name, else a
// placeholder, because a malformed entry may carry neither.
func entryLabel(entry *Patch) string {
	if entry.ID != "" {
		return entry.ID
	}
	if entry.Name != nil && *entry.Name != "" {
		return *entry.Name
	}
	return "<unnamed>"
}

// applyBase creates entries for a base layer.
func (c *treeComposer) applyBase(source string, targetNodes *[]*Node, entry *Patch) error {
	if entry == nil {
		return nil
	}
	baseEntries := entry.Insert
	if len(baseEntries) == 0 {
		baseEntries = []*Patch{entry}
	}
	for _, baseEntry := range baseEntries {
		if baseEntry == nil {
			continue
		}
		node, err := createNode(baseEntry, source, "entry")
		if err != nil {
			return err
		}
		*targetNodes = append(*targetNodes, node)
		// Duplicates are reported by indexNode, which sees nested children too.
		// Keeping one path for every creation site also keeps the outcome
		// independent of the order the entries happen to appear in.
		if err := c.indexNode(node); err != nil {
			return err
		}
	}
	return nil
}

func (c *treeComposer) applyPatch(source string, targetNodes *[]*Node, patch *Patch) error {
	if patch == nil {
		return nil
	}
	if len(patch.Insert) > 0 {
		for _, insertedEntry := range patch.Insert {
			if insertedEntry == nil {
				continue
			}
			if insertedEntry.ID != "" {
				if _, exists := c.index[insertedEntry.ID]; exists {
					message := fmt.Sprintf("layer %s: duplicate entry id %q",
						source, insertedEntry.ID)
					if c.options.strict {
						return fmt.Errorf("%s", message)
					}
					c.tree.Warnings = append(c.tree.Warnings, message)
					continue
				}
			}
			node, err := createNode(insertedEntry, source, "inserted entry")
			if err != nil {
				return err
			}
			*targetNodes = append(*targetNodes, node)
			if err := c.indexNode(node); err != nil {
				return err
			}
		}
		return nil
	}
	if patch.ID == "" {
		return fmt.Errorf("layer %s: entry requires id or insert", source)
	}
	node, ok := c.index[patch.ID]
	if !ok {
		message := fmt.Sprintf("layer %s: patch id %q matched no entry", source, patch.ID)
		if c.options.strict {
			return fmt.Errorf("%s", message)
		}
		c.tree.Warnings = append(c.tree.Warnings, message)
		return nil
	}
	mergePatch(node, patch, source)
	for _, childPatch := range patch.Plugins {
		if err := c.applyPatch(source, &node.Children, childPatch); err != nil {
			return err
		}
	}
	return nil
}

// createNode builds one node from an entry. where locates the entry inside its
// layer - "entry", "inserted entry", "entry plugins[1] insert[0]" - so an
// unusable nested entry can be reported instead of turning into an anonymous
// node that no id addresses and no plugin name resolves.
func createNode(entry *Patch, source, where string) (*Node, error) {
	if entry.ID == "" && !namesEntry(entry) {
		return nil, fmt.Errorf("layer %s: %s requires id or name", source, where)
	}
	node := &Node{ID: entry.ID, Source: source}
	if entry.Name != nil {
		node.Name = *entry.Name
	}
	if entry.Label != nil {
		node.Label = *entry.Label
	}
	if entry.Disabled != nil {
		node.Disabled = *entry.Disabled
	}
	if entry.Group != nil {
		node.Group = *entry.Group
	}
	if entry.Inject != nil {
		node.Inject = append([]string(nil), (*entry.Inject)...)
	}
	if entry.Config != nil {
		node.Config = cloneMap(entry.Config)
	}
	for i, child := range entry.Plugins {
		if child == nil {
			continue
		}
		childWhere := fmt.Sprintf("%s plugins[%d]", where, i)
		if len(child.Insert) > 0 {
			// A nested entry may insert instead of naming a plugin: it expands
			// into sibling children, exactly as applyPatch expands it in a
			// patch layer, so a base layer and a patch layer read the same.
			for j, inserted := range child.Insert {
				if inserted == nil {
					continue
				}
				insertedNode, err := createNode(inserted, source,
					fmt.Sprintf("%s insert[%d]", childWhere, j))
				if err != nil {
					return nil, err
				}
				node.Children = append(node.Children, insertedNode)
			}
			continue
		}
		childNode, err := createNode(child, source, childWhere)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, childNode)
	}
	return node, nil
}

// namesEntry reports whether an entry carries a usable name.
func namesEntry(entry *Patch) bool {
	return entry.Name != nil && *entry.Name != ""
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
//
// The first node keeps the id. Overwriting it would send a later patch to one
// node while Find and Dump keep reporting the other, so the tree would answer
// "what is configured for x?" differently depending on who asks.
func (c *treeComposer) indexNode(node *Node) error {
	if node == nil {
		return nil
	}
	if node.ID != "" {
		if _, exists := c.index[node.ID]; exists {
			message := fmt.Sprintf("layer %s: duplicate entry id %q", node.Source, node.ID)
			if c.options.strict {
				return fmt.Errorf("%s", message)
			}
			c.tree.Warnings = append(c.tree.Warnings, message)
		} else {
			c.index[node.ID] = node
		}
	}
	for _, child := range node.Children {
		if err := c.indexNode(child); err != nil {
			return err
		}
	}
	return nil
}

// cloneMap copies a config map and the containers inside it, so a composed tree
// never aliases the patch it was built from: the same Patch can feed several
// trees, and one tree's config must not change underneath the others. Only the
// shapes a layer file can produce (map[string]any and []any) are copied; any
// other value is shared as-is.
func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = cloneValue(value)
	}
	return out
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = cloneValue(item)
		}
		return out
	default:
		return value
	}
}

// Find returns the node with the given id, searching the whole tree.
func (t *Tree) Find(id string) *Node {
	var walk func([]*Node) *Node
	walk = func(nodes []*Node) *Node {
		for _, node := range nodes {
			if node == nil {
				// A tree built by hand may hold nil entries. Load skips them,
				// so the read-only walkers skip them too.
				continue
			}
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
		total := 0
		for _, node := range nodes {
			if node == nil {
				// Nil entries are not loaded, so they are not counted.
				continue
			}
			total++
			total += count(node.Children)
		}
		return total
	}
	return count(t.Nodes)
}

// Registry maps configuration entry names to plugin definitions.
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]*registeredPlugin
}

// registeredPlugin erases Plugin[C]'s config type behind the loader's
// map-based configuration boundary.
type registeredPlugin struct {
	load func(ctx *cordis.Context, config map[string]any, extra []string) (*cordis.Fiber, error)
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{plugins: map[string]*registeredPlugin{}}
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
	if _, exists := registry.plugins[name]; exists {
		return fmt.Errorf("loader: plugin %q is already registered", name)
	}
	registry.plugins[name] = &registeredPlugin{
		load: func(ctx *cordis.Context, config map[string]any,
			extra []string) (*cordis.Fiber, error) {
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

func (r *Registry) get(name string) (*registeredPlugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	plugin, ok := r.plugins[name]
	return plugin, ok
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
	names := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// decodeConfig turns a map-based config into the plugin's own type by encoding
// it back to JSON, so the target type's UnmarshalJSON runs and its defaults and
// validation apply.
//
// A nil map means "no config" and leaves the target at its zero value. An empty
// map is a config object like any other and must still reach the target type:
// a pointer config gets allocated and a defaulted field gets its default, which
// is exactly what a type with its own UnmarshalJSON expects for `config: {}`.
func decodeConfig[C any](raw map[string]any) (C, error) {
	var config C
	if raw == nil {
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

func (t *Tree) loadNodes(ctx *cordis.Context, registry *Registry, nodes []*Node,
	inherited []string, fibers *[]*cordis.Fiber) error {
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
			if err := t.loadNodes(ctx.Fork(label), registry, node.Children,
				deps, fibers); err != nil {
				return err
			}
			continue
		}
		registeredPlugin, ok := registry.get(node.Name)
		if !ok {
			return fmt.Errorf("loader: entry %q references unknown plugin %q", node.ID, node.Name)
		}
		if len(node.Children) > 0 {
			// Only a group recurses into its children, so loading this entry
			// would silently drop them: refuse instead of half-loading the tree.
			return fmt.Errorf("loader: entry %q (%s): plugins require group: true",
				node.ID, node.Name)
		}
		fiber, err := registeredPlugin.load(ctx, node.Config, deps)
		if err != nil {
			// A failed entry still owns a fiber: it holds a slot in the parent's
			// effect tree and a runtime in the registry. Rollback below only
			// walks the entries that loaded, so dispose this one here - a load
			// error always comes with the fiber that reported it.
			if fiber != nil {
				fiber.Dispose()
			}
			// A failing entry is a configuration error: surface it so the caller
			// sees all-or-nothing instead of a half-loaded tree.
			return fmt.Errorf("loader: entry %q (%s): %w", node.ID, node.Name, err)
		}
		*fibers = append(*fibers, fiber)
	}
	return nil
}
