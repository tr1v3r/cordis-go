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

func value(c *cordis.Context, name string) string {
	got, ok := cordis.Get[*db](c, name)
	if !ok {
		return "<unavailable>"
	}
	return got.String()
}

func main() {
	app := cordis.New()

	// ① A provider can read the service it just provided.
	cordis.Load(app, cordis.Define[struct{}]("self", func(ctx *cordis.Context, _ struct{}) error {
		me := &db{"self"}
		if _, err := cordis.Provide[*db](ctx, "own", me); err != nil {
			return err
		}
		got, ok := cordis.Get[*db](ctx, "own")
		fmt.Printf("① provider sees own service: ok=%v same=%v\n", ok, got == me)
		return nil
	}), struct{}{})

	// ② Two providers, one name: the second one fails and names the owner.
	ownerA, _ := cordis.Load(app, cordis.Define[struct{}]("owner-a", func(ctx *cordis.Context, _ struct{}) error {
		_, err := cordis.Provide[*db](ctx, "db", &db{"A"})
		return err
	}), struct{}{})
	dup, err := cordis.Load(app, cordis.Define[struct{}]("owner-b", func(ctx *cordis.Context, _ struct{}) error {
		_, err := cordis.Provide[*db](ctx, "db", &db{"B"})
		return err
	}), struct{}{})
	fmt.Printf("② duplicate provide: state=%s err=%v\n", dup.State(), err)

	// ③ A dependent follows the provider's *state*, not the store entry.
	runs := 0
	dependent, _ := cordis.Load(app, cordis.Define[struct{}]("dependent", func(ctx *cordis.Context, _ struct{}) error {
		runs++
		fmt.Printf("③ dependent ran, saw=%s\n", value(ctx, "db"))
		return nil
	}).WithInject("db"), struct{}{})
	fmt.Printf("③ dependent=%s runs=%d\n", dependent.State(), runs)

	ownerA.Dispose()
	fmt.Printf("③ provider disposed      -> dependent=%s\n", dependent.State())

	cordis.Load(app, cordis.Define[struct{}]("owner-a2", func(ctx *cordis.Context, _ struct{}) error {
		_, err := cordis.Provide[*db](ctx, "db", &db{"A2"})
		return err
	}), struct{}{})
	fmt.Printf("③ provider provided again-> dependent=%s runs=%d\n", dependent.State(), runs)

	// ④ Set replaces the value without changing the provider's identity,
	// so dependents are notified but do not reload.
	var cacheCtx *cordis.Context
	cordis.Load(app, cordis.Define[struct{}]("cache", func(ctx *cordis.Context, _ struct{}) error {
		cacheCtx = ctx
		_, err := cordis.Provide[*db](ctx, "cached", &db{"v1"})
		return err
	}), struct{}{})
	consumed := 0
	cordis.Load(app, cordis.Define[struct{}]("consumer", func(ctx *cordis.Context, _ struct{}) error {
		consumed++
		return nil
	}).WithInject("cached"), struct{}{})
	fmt.Printf("④ before Set: consumer runs=%d value=%s\n", consumed, value(app, "cached"))
	if err := cacheCtx.Set("cached", &db{"v2"}); err != nil {
		fmt.Printf("④ Set failed: %v\n", err)
	}
	fmt.Printf("④ after  Set: consumer runs=%d value=%s (value new, no reload)\n", consumed, value(app, "cached"))

	// ⑤ A failing availability predicate hides the service; flipping it back
	// does not wake dependents on its own.
	var ready atomic.Bool
	var flakyCtx *cordis.Context
	cordis.Load(app, cordis.Define[struct{}]("flaky", func(ctx *cordis.Context, _ struct{}) error {
		flakyCtx = ctx
		_, err := cordis.ProvideChecked[*db](ctx, "flaky", &db{"f"}, func() bool { return ready.Load() })
		return err
	}), struct{}{})
	waiter, _ := cordis.Load(app, cordis.Define[struct{}]("waiter", func(ctx *cordis.Context, _ struct{}) error {
		fmt.Println("⑤ waiter ran")
		return nil
	}).WithInject("flaky"), struct{}{})
	fmt.Printf("⑤ check=false -> waiter=%s\n", waiter.State())
	ready.Store(true)
	fmt.Printf("⑤ check=true  -> waiter=%s (predicate flip alone wakes nobody)\n", waiter.State())
	if err := flakyCtx.Set("flaky", &db{"f"}); err != nil {
		fmt.Printf("⑤ Set failed: %v\n", err)
	}
	fmt.Printf("⑤ after Set   -> waiter=%s\n", waiter.State())

	// ⑥ Serve runs the Start/Stop hooks around the registration.
	svc, _ := cordis.Load(app, cordis.Define[struct{}]("greeter", func(ctx *cordis.Context, _ struct{}) error {
		_, err := cordis.Serve[*greeter](ctx, "greeter", &greeter{"greeter"})
		return err
	}), struct{}{})
	svc.Dispose()

	app.Fiber().Dispose()
}
