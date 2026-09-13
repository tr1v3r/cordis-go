// Command hotplug demonstrates replacing a plugin while the application keeps
// running. The dependent plugin pauses when its provider disappears and resumes
// automatically after a different plugin provides the same service.
package main

import (
	"fmt"
	"sort"
	"strings"

	cordis "github.com/tr1v3r/cordis-go"
)

// Greeter is the service the client consumes; the demo swaps implementations
// of it while the application keeps running.
type Greeter interface {
	Version() string
	Greet(string) string
}

// greeter is one Greeter implementation, built from a version and a greeting.
type greeter struct {
	version string
	greet   func(string) string
}

func (g *greeter) Version() string          { return g.version }
func (g *greeter) Greet(name string) string { return g.greet(name) }

// greetRequest is the event payload the client plugin answers.
type greetRequest struct {
	Name string
}

func main() {
	rootCtx := cordis.New()
	registry := rootCtx.MustGet[cordis.Registry]("registry")

	activations := 0
	clientPlugin := cordis.Define[struct{}]("greeter-client",
		func(ctx *cordis.Context, _ struct{}) error {
			activations++
			activation := activations
			greeter := ctx.MustGet[Greeter]("greeter")
			fmt.Printf("client activation #%d uses %s\n", activation, greeter.Version())

			ctx.OnValue("greet", func(request greetRequest) any {
				return greeter.Greet(request.Name)
			})
			ctx.OnDispose(func() {
				fmt.Printf("client activation #%d stopped\n", activation)
			})
			return nil
		}).WithInject("greeter")

	legacyPlugin := newGreeterPlugin("greeter-v1", &greeter{
		version: "v1",
		greet:   func(name string) string { return "hello, " + name },
	})

	fmt.Println("== initial registration ==")
	legacyFiber := mustLoad(rootCtx.Load(legacyPlugin, struct{}{}))
	clientFiber := mustLoad(rootCtx.Load(clientPlugin, struct{}{}))
	printState(legacyFiber, clientFiber, registry)
	requestGreeting(rootCtx, "Cordis")

	fmt.Println("\n== provider disappears while the app keeps running ==")
	legacyFiber.Dispose()
	printState(legacyFiber, clientFiber, registry)
	requestGreeting(rootCtx, "Cordis")

	replacementPlugin := newGreeterPlugin("greeter-v2", &greeter{
		version: "v2",
		greet: func(name string) string {
			return "WELCOME, " + strings.ToUpper(name)
		},
	})

	fmt.Println("\n== register a replacement plugin ==")
	replacementFiber := mustLoad(rootCtx.Load(replacementPlugin, struct{}{}))
	printState(replacementFiber, clientFiber, registry)
	requestGreeting(rootCtx, "Cordis")

	rootCtx.Fiber().Dispose()
}

func newGreeterPlugin(name string, service Greeter) *cordis.Plugin[struct{}] {
	return cordis.Define[struct{}](name, func(ctx *cordis.Context, _ struct{}) error {
		_, err := ctx.Provide("greeter", service)
		return err
	})
}

func requestGreeting(ctx *cordis.Context, name string) {
	result, handled := ctx.Bail("greet", greetRequest{Name: name})
	if !handled {
		fmt.Printf("request %q -> unavailable\n", name)
		return
	}
	fmt.Printf("request %q -> %q\n", name, result)
}

func printState(provider, client *cordis.Fiber, registry cordis.Registry) {
	plugins := registry.Plugins()
	sort.Strings(plugins)
	fmt.Printf("provider=%s client=%s registry=%v\n", provider.State(), client.State(), plugins)
}

func mustLoad(fiber *cordis.Fiber, err error) *cordis.Fiber {
	if err != nil {
		panic(err)
	}
	return fiber
}
