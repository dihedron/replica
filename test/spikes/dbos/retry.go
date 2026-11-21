package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

var errPermanent = errors.New("permanent")
var attempts = map[string]int{}

func Flaky(ctx dbos.Context, kind string) (string, error) {
	t0 := time.Now()
	return dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		attempts[kind]++
		ev("%s attempt %d at +%s", kind, attempts[kind], time.Since(t0).Round(time.Millisecond))
		if kind == "permanent" {
			return "", errPermanent
		}
		if attempts[kind] < 5 {
			return "", errors.New("transient")
		}
		return "ok", nil
	}, dbos.WithStepName("flaky-"+kind), dbos.WithStepMaxRetries(10), dbos.WithStepBaseInterval(200*time.Millisecond),
		dbos.WithStepBackoffFactor(2), dbos.WithStepMaxInterval(time.Second),
		dbos.WithStepRetryPredicate(func(err error) bool { return !errors.Is(err, errPermanent) }))
}

func runRetry(dsn string) {
	ctx, err := dbos.NewContext(context.Background(), dbos.Config{AppName: "spike-retry", DatabaseURL: dsn, ExecutorID: "r", Logger: quiet})
	must(err)
	dbos.RegisterWorkflow(ctx, Flaky)
	must(dbos.Launch(ctx))
	for _, k := range []string{"transient", "permanent"} {
		h, err := dbos.RunWorkflow(ctx, Flaky, k)
		must(err)
		r, err := h.GetResult()
		fmt.Printf("%s: result=%q err=%v\n", k, r, err)
	}
	_ = dbos.Shutdown(ctx, 3*time.Second)
}

var _ = os.Args
