// Package cordis is a Go port of the Cordis meta-framework (v4), the plugin
// system that powers Koishi and DeepSeek Harness.
//
// The original Cordis is a TypeScript framework built around three ideas:
// a context tree, a per-plugin lifecycle state machine ("fiber"), and
// reversible side effects. This port keeps those semantics while replacing the
// JavaScript-specific machinery with Go idioms:
//
//   - Dynamic property access (ctx.foo) becomes explicit, type-safe lookups:
//     cordis.Get[*DB](ctx, "db").
//   - Plugin shapes (function / class / { apply }) become *cordis.Plugin[C]
//     values with a typed config.
//   - Promises become synchronous calls; goroutines are cancelled through the
//     context.Context returned by Context.Context.
//   - Module hot replacement (import()) has no in-process equivalent in Go, so
//     hot reload is scoped to configuration and plugin lifecycle, not code.
//
// A plugin declares the services it needs through Inject. Until every required
// service is provided by an active fiber, the plugin stays PENDING; when a
// dependency appears, changes provider, or disappears, the plugin is
// automatically unloaded and reloaded. Every side effect registered through the
// context (services, event listeners, arbitrary disposers) is unwound in
// reverse order when the plugin unloads, which is what makes plugins
// reversible.
package cordis
