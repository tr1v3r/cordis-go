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

func (s *Server) Stop() error {
	fmt.Printf("server %s stopped\n", s.addr)
	return nil
}

// Ping is an event payload.
type Ping struct{ From string }

const baseLayer = `[
  {"id": "db", "name": "db", "config": {"path": "data/app.db"}},
  {"id": "server", "name": "server", "config": {"addr": ":8080", "tls": true}}
]`

const profileLayer = `[
  {"id": "server", "config": {"addr": ":9090"}}
]`

func main() {
	registry := loader.NewRegistry()

	loader.MustRegister(registry, "db", cordis.Define[dbConfig]("db", func(ctx *cordis.Context, cfg dbConfig) error {
		database := &DB{path: cfg.Path}
		if _, err := cordis.Serve(ctx, "db", database); err != nil {
			return err
		}
		ctx.Logger().Info("db ready at %s", cfg.Path)
		return nil
	}))

	loader.MustRegister(registry, "server", cordis.Define[serverConfig]("server", func(ctx *cordis.Context, cfg serverConfig) error {
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
		cordis.On[*Ping](ctx, "ping", func(ping *Ping) {
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

	root := cordis.New(cordis.WithWriter(os.Stdout), cordis.WithLevel(cordis.LevelInfo))

	fmt.Println("== load ==")
	fibers, err := tree.Load(root, registry)
	must(err)
	for _, fiber := range fibers {
		fmt.Printf("fiber %-8s state=%s deps=%v\n", fiber.Name(), fiber.State(), fiber.Inject())
	}

	fmt.Println("== event ==")
	cordis.Emit[*Ping](root, "ping", &Ping{From: "cli"})

	// A plugin that needs a service nobody provides stays pending instead of
	// failing; it activates as soon as the service appears.
	pending, err := cordis.Load(root, cordis.Define[struct{}]("cache", func(ctx *cordis.Context, _ struct{}) error {
		ctx.Logger().Info("cache started")
		return nil
	}).WithInject("cache"), struct{}{})
	must(err)
	fmt.Printf("fiber %-8s state=%s\n", pending.Name(), pending.State())
	if _, err := cordis.Provide[*DB](root, "cache", &DB{path: "cache.db"}); err != nil {
		must(err)
	}
	fmt.Printf("fiber %-8s state=%s\n", pending.Name(), pending.State())

	fmt.Println("== dispose ==")
	root.Fiber().Dispose()
	_, alive := cordis.Get[*Server](root, "server")
	fmt.Printf("server service still available: %v\n", alive)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
