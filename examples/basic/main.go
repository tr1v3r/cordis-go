// Command basic demonstrates the whole cordis-go story: typed plugins, a
// config-driven tree with patch layers, a config dump, dependency injection,
// scoped events, and reversible teardown.
package main

import (
	"fmt"
	"os"

	cordis "github.com/tr1v3r/cordis-go"
	"github.com/tr1v3r/cordis-go/loader"
)

type dbConfig struct {
	Path string `json:"path"`
}

type serverConfig struct {
	Addr string `json:"addr"`
}

// DB is a service value. It has no lifecycle hooks, so Serve just registers it.
type DB struct{ path string }

// Server has a Stop hook, which Serve runs when the owning fiber unloads.
type Server struct {
	addr string
}

// Stop is the shutdown hook Serve runs when the owning fiber unloads.
func (s *Server) Stop() error {
	fmt.Printf("server %s stopped\n", s.addr)
	return nil
}

// Ping is an event payload.
type Ping struct{ From string }

const baseLayer = `[
  {"id": "primary-db", "name": "database-module", "config": {"path": "data/app.db"}},
  {"id": "api-server", "name": "http-module", "config": {"addr": ":8080", "tls": true}}
]`

const profileLayer = `[
  {"id": "api-server", "config": {"addr": ":9090"}}
]`

func main() {
	registry := loader.NewRegistry()

	// Registry names select plugin definitions from config; they are independent
	// from the diagnostic plugin names and the services those plugins provide.
	loader.MustRegister(registry, "database-module",
		cordis.Define("database-provider", func(ctx *cordis.Context, cfg dbConfig) error {
			database := &DB{path: cfg.Path}
			if _, err := cordis.Serve(ctx, "db", database); err != nil {
				return err
			}
			ctx.Logger().Info("db ready at %s", cfg.Path)
			return nil
		}))

	loader.MustRegister(registry, "http-module",
		cordis.Define("http-server", func(ctx *cordis.Context, cfg serverConfig) error {
			// The db dependency is declared below via WithInject, so this plugin only
			// runs once the db service exists.
			database, ok := cordis.Get[*DB](ctx, "db")
			if !ok {
				return fmt.Errorf("db service unavailable")
			}
			server := &Server{addr: cfg.Addr}
			if _, err := cordis.Serve(ctx, "server", server); err != nil {
				return err
			}
			ctx.On("ping", func(ping *Ping) {
				ctx.Logger().Info("%s -> %s (db=%s)", ping.From, server.addr, database.path)
			})
			return nil
		}).WithInject("db"))

	base, err := loader.ParseLayer("base", []byte(baseLayer))
	must(err)
	profile, err := loader.ParsePatchLayer("profile", []byte(profileLayer))
	must(err)

	tree, err := loader.Compose([]loader.Layer{base, profile}, loader.Strict())
	must(err)

	fmt.Println("== composed config ==")
	must(tree.Dump(os.Stdout))

	rootCtx := cordis.New(cordis.WithWriter(os.Stdout), cordis.WithLevel(cordis.LevelInfo))

	fmt.Println("== load ==")
	fibers, err := tree.Load(rootCtx, registry)
	must(err)
	for _, fiber := range fibers {
		fmt.Printf("fiber %-8s state=%s deps=%v\n", fiber.Name(), fiber.State(), fiber.Inject())
	}

	fmt.Println("== event ==")
	rootCtx.Emit("ping", &Ping{From: "cli"})

	// A plugin that needs a service nobody provides stays pending instead of
	// failing; it activates as soon as the service appears.
	cacheConsumerFiber, err := rootCtx.Load(
		cordis.Define("cache-consumer", func(ctx *cordis.Context, _ struct{}) error {
			ctx.Logger().Info("cache consumer started")
			return nil
		}).WithInject("cache"), struct{}{})
	must(err)
	fmt.Printf("fiber %-14s state=%s\n", cacheConsumerFiber.Name(), cacheConsumerFiber.State())
	if _, err := cordis.Provide[*DB](rootCtx, "cache", &DB{path: "cache.db"}); err != nil {
		must(err)
	}
	fmt.Printf("fiber %-14s state=%s\n", cacheConsumerFiber.Name(), cacheConsumerFiber.State())

	fmt.Println("== dispose ==")
	rootCtx.Fiber().Dispose()
	_, alive := cordis.Get[*Server](rootCtx, "server")
	fmt.Printf("server service still available: %v\n", alive)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
