// Command spike is a throw-away harness used to resolve the [VERIFY] items in
// docs/specs/07-open-questions.md against dbos-transact-golang v0.11.0.
//
//	spike worker  <executorID> <adminPort> <dsn> [parts]  run an executor
//	spike enqueue <dsn> <queue> <wfID> [partitionKey] [parts]
//	spike retry   <dsn>                                  step-retry experiment
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
)

type Job struct {
	ID    string
	Parts int
	Sleep int // ms per part
}

var logPath = "/var/tmp/pgspike/events.log"

func ev(format string, a ...any) {
	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

var execID string

func Transfer(ctx dbos.DBOSContext, in Job) (string, error) {
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

func main() {
	switch os.Args[1] {
	case "worker":
		execID = os.Args[2]
		port, _ := strconv.Atoi(os.Args[3])
		dsn := os.Args[4]
		sleep := 1000
		if len(os.Args) > 5 {
			sleep, _ = strconv.Atoi(os.Args[5])
		}
		_ = sleep
		ctx, err := dbos.NewDBOSContext(context.Background(), dbos.Config{
			AppName: "spike", DatabaseURL: dsn, ExecutorID: execID,
			AdminServer: true, AdminServerPort: port, ApplicationVersion: "v1",
			Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		})
		must(err)
		dbos.RegisterWorkflow(ctx, Transfer)
		dbos.NewWorkflowQueue(ctx, "transfers")
		dbos.NewWorkflowQueue(ctx, "part", dbos.WithPartitionQueue(), dbos.WithGlobalConcurrency(1))
		must(dbos.Launch(ctx))
		ev("[%s] launched", execID)
		if dead := os.Getenv("ADOPT"); dead != "" {
			go func() {
				time.Sleep(2 * time.Second)
				wfs, err := dbos.ListWorkflows(ctx, dbos.WithExecutorIDs([]string{dead}), dbos.WithStatus([]dbos.WorkflowStatusType{dbos.WorkflowStatusPending}))
				ev("[%s] adopt: list(%s) -> %d workflows err=%v", execID, dead, len(wfs), err)
				for _, w := range wfs {
					_, err := dbos.ResumeWorkflow[any](ctx, w.ID)
					ev("[%s] adopt: ResumeWorkflow(%s) err=%v", execID, w.ID, err)
				}
			}()
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		dbos.Shutdown(ctx, 5*time.Second)
	case "retry":
		runRetry(os.Args[2])
	case "enqueue":
		c, err := dbos.NewClient(context.Background(), dbos.ClientConfig{DatabaseURL: os.Args[2]})
		must(err)
		defer c.Shutdown(5 * time.Second)
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
		h, err := dbos.Enqueue[Job, string](c.(any).(dbos.Client), os.Args[3], "main.Transfer", Job{ID: os.Args[4], Parts: parts, Sleep: sleep}, opts...)
		must(err)
		fmt.Println("enqueued", h.GetWorkflowID())
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
