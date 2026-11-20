package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

var attempts int

func Flaky(ctx dbos.DBOSContext, _ string) (string, error) {
	t0 := time.Now()
	out, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		attempts++
		ev("flaky attempt %d at +%s", attempts, time.Since(t0).Round(time.Millisecond))
		if attempts < 5 {
			return "", errors.New("transient")
		}
		return "ok", nil
	}, dbos.WithStepName("flaky"), dbos.WithStepMaxRetries(10), dbos.WithBaseInterval(200*time.Millisecond),
		dbos.WithBackoffFactor(2), dbos.WithMaxInterval(1*time.Second))
	return out, err
}

func runRetry(dsn string) {
	ctx, err := dbos.NewDBOSContext(context.Background(), dbos.Config{AppName: "spike2", DatabaseURL: dsn, ExecutorID: "r",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))})
	must(err)
	dbos.RegisterWorkflow(ctx, Flaky)
	must(dbos.Launch(ctx))
	h, err := dbos.RunWorkflow(ctx, Flaky, "x")
	must(err)
	r, err := h.GetResult()
	fmt.Println("result:", r, err)
	dbos.Shutdown(ctx, 3*time.Second)
}
