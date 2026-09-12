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

type Greeter interface {
	Version() string
	Greet(string) string
}

type greeter struct {
	version string
	greet   func(string) string
}

func (g *greeter) Version() string          { return g.version }
func (g *greeter) Greet(name string) string { return g.greet(name) }

type greetRequest struct {
	Name string
}

func main() {
	rootCtx := cordis.New()
	registry := cordis.MustGet[cordis.Registry](rootCtx, "registry")

	activations := 0
	clientPlugin := cordis.Define[struct{}]("greeter-client",
		func(ctx *cordis.Context, _ struct{}) error {
			activations++
			activation := activations
			greeter := cordis.MustGet[Greeter](ctx, "greeter")
			fmt.Printf("client activation #%d uses %s\n", activation, greeter.Version())

			cordis.OnValue[greetRequest](ctx, "greet", func(request greetRequest) any {
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
	legacyFiber := mustLoad(cordis.Load(rootCtx, legacyPlugin, struct{}{}))
	clientFiber := mustLoad(cordis.Load(rootCtx, clientPlugin, struct{}{}))
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
	replacementFiber := mustLoad(cordis.Load(rootCtx, replacementPlugin, struct{}{}))
	printState(replacementFiber, clientFiber, registry)
	requestGreeting(rootCtx, "Cordis")

	rootCtx.Fiber().Dispose()
}

func newGreeterPlugin(name string, service Greeter) *cordis.Plugin[struct{}] {
	return cordis.Define[struct{}](name, func(ctx *cordis.Context, _ struct{}) error {
		_, err := cordis.Provide[Greeter](ctx, "greeter", service)
		return err
	})
}

func requestGreeting(ctx *cordis.Context, name string) {
	result, handled := cordis.Bail(ctx, "greet", greetRequest{Name: name})
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
