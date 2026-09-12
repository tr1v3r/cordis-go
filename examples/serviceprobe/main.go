// Command serviceprobe exercises the service container's observable rules.
// It is a scratch probe used to ground the design notes; run it with
// `go run ./examples/serviceprobe`.
package main

import (
	"fmt"
	"sync/atomic"

	"github.com/tr1v3r/cordis-go"
)

type db struct{ id string }

func (d *db) String() string { return d.id }

type greeter struct{ name string }

func (g *greeter) Start() error { fmt.Printf("⑥ %s: Start\n", g.name); return nil }
func (g *greeter) Stop() error  { fmt.Printf("⑥ %s: Stop\n", g.name); return nil }

func value(ctx *cordis.Context, name string) string {
	got, ok := cordis.Get[*db](ctx, name)
	if !ok {
		return "<unavailable>"
	}
	return got.String()
}

func main() {
	rootCtx := cordis.New()

	// ① A provider can read the service it just provided.
	cordis.Load(rootCtx,
		cordis.Define[struct{}]("self", func(ctx *cordis.Context, _ struct{}) error {
			me := &db{"self"}
			if _, err := cordis.Provide[*db](ctx, "own", me); err != nil {
				return err
			}
			got, ok := cordis.Get[*db](ctx, "own")
			fmt.Printf("① provider sees own service: ok=%v same=%v\n", ok, got == me)
			return nil
		}), struct{}{})

	// ② Two providers, one name: the second one fails and names the owner.
	providerFiberA, _ := cordis.Load(rootCtx,
		cordis.Define[struct{}]("owner-a", func(ctx *cordis.Context, _ struct{}) error {
			_, err := cordis.Provide[*db](ctx, "db", &db{"A"})
			return err
		}), struct{}{})
	duplicateProviderFiber, err := cordis.Load(rootCtx,
		cordis.Define[struct{}]("owner-b", func(ctx *cordis.Context, _ struct{}) error {
			_, err := cordis.Provide[*db](ctx, "db", &db{"B"})
			return err
		}), struct{}{})
	fmt.Printf("② duplicate provide: state=%s err=%v\n", duplicateProviderFiber.State(), err)

	// ③ A dependent follows the provider's *state*, not the store entry.
	runs := 0
	dependentFiber, _ := cordis.Load(rootCtx,
		cordis.Define[struct{}]("dependent", func(ctx *cordis.Context, _ struct{}) error {
			runs++
			fmt.Printf("③ dependent ran, saw=%s\n", value(ctx, "db"))
			return nil
		}).WithInject("db"), struct{}{})
	fmt.Printf("③ dependent=%s runs=%d\n", dependentFiber.State(), runs)

	providerFiberA.Dispose()
	fmt.Printf("③ provider disposed      -> dependent=%s\n", dependentFiber.State())

	cordis.Load(rootCtx,
		cordis.Define[struct{}]("owner-a2", func(ctx *cordis.Context, _ struct{}) error {
			_, err := cordis.Provide[*db](ctx, "db", &db{"A2"})
			return err
		}), struct{}{})
	fmt.Printf("③ provider provided again-> dependent=%s runs=%d\n", dependentFiber.State(), runs)

	// ④ Set replaces the value without changing the provider's identity,
	// so dependents are notified but do not reload.
	var cacheCtx *cordis.Context
	cordis.Load(rootCtx,
		cordis.Define[struct{}]("cache", func(ctx *cordis.Context, _ struct{}) error {
			cacheCtx = ctx
			_, err := cordis.Provide[*db](ctx, "cached", &db{"v1"})
			return err
		}), struct{}{})
	consumed := 0
	cordis.Load(rootCtx,
		cordis.Define[struct{}]("consumer", func(_ *cordis.Context, _ struct{}) error {
			consumed++
			return nil
		}).WithInject("cached"), struct{}{})
	fmt.Printf("④ before Set: consumer runs=%d value=%s\n", consumed, value(rootCtx, "cached"))
	if err := cacheCtx.Set("cached", &db{"v2"}); err != nil {
		fmt.Printf("④ Set failed: %v\n", err)
	}
	fmt.Printf("④ after  Set: consumer runs=%d value=%s (value new, no reload)\n",
		consumed, value(rootCtx, "cached"))

	// ⑤ A failing availability predicate hides the service; flipping it back
	// does not wake dependents on its own.
	var ready atomic.Bool
	var flakyCtx *cordis.Context
	cordis.Load(rootCtx,
		cordis.Define[struct{}]("flaky", func(ctx *cordis.Context, _ struct{}) error {
			flakyCtx = ctx
			_, err := cordis.ProvideChecked[*db](ctx, "flaky", &db{"f"},
				func() bool { return ready.Load() })
			return err
		}), struct{}{})
	waiterFiber, _ := cordis.Load(rootCtx,
		cordis.Define[struct{}]("waiter", func(_ *cordis.Context, _ struct{}) error {
			fmt.Println("⑤ waiter ran")
			return nil
		}).WithInject("flaky"), struct{}{})
	fmt.Printf("⑤ check=false -> waiter=%s\n", waiterFiber.State())
	ready.Store(true)
	fmt.Printf("⑤ check=true  -> waiter=%s (predicate flip alone wakes nobody)\n",
		waiterFiber.State())
	if err := flakyCtx.Set("flaky", &db{"f"}); err != nil {
		fmt.Printf("⑤ Set failed: %v\n", err)
	}
	fmt.Printf("⑤ after Set   -> waiter=%s\n", waiterFiber.State())

	// ⑥ Serve runs the Start/Stop hooks around the registration.
	greeterFiber, _ := cordis.Load(rootCtx,
		cordis.Define[struct{}]("greeter", func(ctx *cordis.Context, _ struct{}) error {
			_, err := cordis.Serve[*greeter](ctx, "greeter", &greeter{"greeter"})
			return err
		}), struct{}{})
	greeterFiber.Dispose()

	rootCtx.Fiber().Dispose()
}
