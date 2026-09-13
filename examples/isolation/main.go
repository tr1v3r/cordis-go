// Command isolation demonstrates default, isolated, and explicitly shared
// service scopes.
package main

import (
	"fmt"

	cordis "github.com/tr1v3r/cordis-go"
)

type database struct {
	label string
}

type databaseConfig struct {
	Label string
}

type consumerConfig struct {
	Label string
}

var databasePlugin = cordis.Define("database-provider",
	func(ctx *cordis.Context, cfg databaseConfig) error {
		_, err := ctx.Provide("db", &database{label: cfg.Label})
		return err
	})

var consumerPlugin = cordis.Define("database-consumer",
	func(ctx *cordis.Context, cfg consumerConfig) error {
		db, ok := ctx.Get[*database]("db")
		if !ok {
			return fmt.Errorf("db service unavailable")
		}
		fmt.Printf("%-18s uses %s\n", cfg.Label, db.label)
		return nil
	}).WithInject("db")

func main() {
	rootCtx := cordis.New()
	defer rootCtx.Fiber().Dispose()

	// Without isolation, providers and consumers resolve the default "db" scope.
	mustLoad(rootCtx, databasePlugin, databaseConfig{Label: "global-db"})
	mustLoad(rootCtx, consumerPlugin, consumerConfig{Label: "root consumer"})

	// Each Isolate call creates a fresh scope for the named service. The returned
	// Context must be used by both the provider and its consumers.
	tenantACtx := rootCtx.Fork("tenant-a").Isolate("db")
	tenantBCtx := rootCtx.Fork("tenant-b").Isolate("db")

	// A consumer loaded before its isolated provider waits in Pending. Publishing
	// "db" in the same isolated Context activates it synchronously.
	tenantAConsumer := mustLoad(tenantACtx, consumerPlugin,
		consumerConfig{Label: "tenant-a consumer"})
	fmt.Printf("tenant-a before provider: %s\n", tenantAConsumer.State())
	mustLoad(tenantACtx, databasePlugin, databaseConfig{Label: "tenant-a-db"})
	fmt.Printf("tenant-a after provider:  %s\n", tenantAConsumer.State())

	mustLoad(tenantBCtx, databasePlugin, databaseConfig{Label: "tenant-b-db"})
	mustLoad(tenantBCtx, consumerPlugin, consumerConfig{Label: "tenant-b consumer"})

	// IsolateShared lets separate Context branches join one explicitly named
	// scope. Include the service name in the label because labels are application
	// wide in the current implementation.
	const tenantCScope = "tenant-c:db"
	tenantCProviderCtx := rootCtx.Fork("tenant-c-provider").IsolateShared("db", tenantCScope)
	tenantCConsumerCtx := rootCtx.Fork("tenant-c-consumer").IsolateShared("db", tenantCScope)
	mustLoad(tenantCProviderCtx, databasePlugin, databaseConfig{Label: "tenant-c-db"})
	mustLoad(tenantCConsumerCtx, consumerPlugin, consumerConfig{Label: "tenant-c consumer"})

	printResolvedDB("root lookup", rootCtx)
	printResolvedDB("tenant-a lookup", tenantACtx)
	printResolvedDB("tenant-b lookup", tenantBCtx)
	printResolvedDB("tenant-c lookup", tenantCConsumerCtx)
}

func mustLoad[C any](ctx *cordis.Context, plugin *cordis.Plugin[C], config C) *cordis.Fiber {
	fiber, err := ctx.Load(plugin, config)
	if err != nil {
		panic(err)
	}
	return fiber
}

func printResolvedDB(label string, ctx *cordis.Context) {
	db, ok := ctx.Get[*database]("db")
	if !ok {
		panic("db service unavailable")
	}
	fmt.Printf("%-18s resolves %s\n", label, db.label)
}
