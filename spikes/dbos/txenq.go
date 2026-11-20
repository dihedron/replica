package main

import (
	"context"
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// runTxEnqueue inserts an application row and enqueues a workflow in ONE
// transaction, then commits or rolls back, and reports what is visible.
func runTxEnqueue(dsn, mode, wfID string) {
	bg := context.Background()
	pool := txPool(dsn)
	defer pool.Close()
	_, err := pool.Exec(bg, "create table if not exists app_jobs(id text primary key, state text)")
	must(err)
	c, err := dbos.NewClient(bg, dbos.ClientConfig{DatabaseURL: dsn, AppName: "spike", Logger: quiet})
	must(err)
	defer dbos.Shutdown(c, 5e9)

	tx, err := pool.Begin(bg)
	must(err)
	_, err = tx.Exec(bg, "insert into app_jobs values ($1,'ACCEPTED')", wfID)
	must(err)
	_, err = dbos.Enqueue[string](c, "transfers", "main.Transfer", Job{ID: wfID, Parts: 2, Sleep: 200},
		dbos.WithEnqueueWorkflowID(wfID), dbos.WithEnqueueApplicationVersion("v1"), dbos.WithEnqueueTransaction(tx))
	must(err)
	if mode == "commit" {
		must(tx.Commit(bg))
	} else {
		must(tx.Rollback(bg))
	}
	var n, m int
	must(pool.QueryRow(bg, "select count(*) from app_jobs where id=$1", wfID).Scan(&n))
	must(pool.QueryRow(bg, "select count(*) from dbos.workflow_status where workflow_uuid=$1", wfID).Scan(&m))
	fmt.Printf("%s: app_jobs rows=%d dbos workflows=%d\n", mode, n, m)
}
