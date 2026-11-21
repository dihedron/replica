// Command spike is a throw-away harness used to resolve the [VERIFY] items in
// docs/specs against dbos-transact-golang (re-run on v1.9.0).
//
//	spike worker  <executorID> <dsn>            run an executor (ADOPT=<id> adopts that executor's PENDING work)
//	spike enqueue <dsn> <queue> <wfID> [partitionKey|-] [parts] [ms]
//	spike txenq   <dsn> <commit|rollback> <wfID> transactional enqueue test
//	spike retry   <dsn>                         step retry predicate test
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Job struct {
	ID    string
	Parts int
	Sleep int // ms per part
}

var logPath = "/var/tmp/pgspike/events.log"

func ev(format string, a ...any) {
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

var execID string

func Transfer(ctx dbos.Context, in Job) (string, error) {
	ev("[%s] workflow %s start", execID, in.ID)
	for i := 0; i < in.Parts; i++ {
		_, err := dbos.RunAsStep(ctx, func(c context.Context) (int, error) {
			ev("[%s] %s part %d begin", execID, in.ID, i)
			time.Sleep(time.Duration(in.Sleep) * time.Millisecond)
			ev("[%s] %s part %d end", execID, in.ID, i)
			return i, nil
		}, dbos.WithStepName(fmt.Sprintf("part-%d", i)))
		if err != nil {
			return "", err
		}
	}
	ev("[%s] workflow %s done", execID, in.ID)
	return "ok", nil
}

var quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

func main() {
	switch os.Args[1] {
	case "worker":
		execID = os.Args[2]
		dsn := os.Args[3]
		ctx, err := dbos.NewContext(context.Background(), dbos.Config{
			AppName: "spike", DatabaseURL: dsn, ExecutorID: execID, ApplicationVersion: "v1", Logger: quiet,
		})
		must(err)
		dbos.RegisterWorkflow(ctx, Transfer)
		_, err = dbos.RegisterQueue(ctx, "transfers")
		must(err)
		_, err = dbos.RegisterQueue(ctx, "part", dbos.WithPartitionConcurrency(1))
		must(err)
		must(dbos.Launch(ctx))
		ev("[%s] launched", execID)
		if dead := os.Getenv("ADOPT"); dead != "" {
			go func() {
				time.Sleep(2 * time.Second)
				wfs, err := dbos.ListWorkflows(ctx, dbos.WithFilterExecutorIDs(dead), dbos.WithFilterStatus(dbos.WorkflowStatusPending))
				ev("[%s] adopt: list(%s) -> %d workflows err=%v", execID, dead, len(wfs), err)
				ids := []string{}
				for _, w := range wfs {
					ids = append(ids, w.ID)
				}
				if len(ids) > 0 {
					_, err := dbos.ResumeWorkflows[any](ctx, ids)
					ev("[%s] adopt: ResumeWorkflows(%v) err=%v", execID, ids, err)
				}
			}()
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		_ = dbos.Shutdown(ctx, 5*time.Second)
	case "enqueue":
		c, err := dbos.NewClient(context.Background(), dbos.ClientConfig{DatabaseURL: os.Args[2], AppName: "spike", Logger: quiet})
		must(err)
		defer func() { _ = dbos.Shutdown(c, 5*time.Second) }()
		opts := []dbos.EnqueueOption{dbos.WithEnqueueWorkflowID(os.Args[4]), dbos.WithEnqueueApplicationVersion("v1")}
		if len(os.Args) > 5 && os.Args[5] != "-" {
			opts = append(opts, dbos.WithEnqueueQueuePartitionKey(os.Args[5]))
		}
		parts, sleep := 20, 1000
		if len(os.Args) > 6 {
			parts, _ = strconv.Atoi(os.Args[6])
		}
		if len(os.Args) > 7 {
			sleep, _ = strconv.Atoi(os.Args[7])
		}
		h, err := dbos.Enqueue[string](c, os.Args[3], "main.Transfer", Job{ID: os.Args[4], Parts: parts, Sleep: sleep}, opts...)
		must(err)
		fmt.Println("enqueued", h.GetWorkflowID())
	case "txenq":
		runTxEnqueue(os.Args[2], os.Args[3], os.Args[4])
	case "retry":
		runRetry(os.Args[2])
	}
}

func txPool(dsn string) *pgxpool.Pool {
	p, err := pgxpool.New(context.Background(), dsn)
	must(err)
	return p
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
