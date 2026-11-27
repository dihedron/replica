// Command envelope is the AC-24 campaign's planner and reporter, for
// test/lab/lib/envelope.sh (.specs/veho-latency-campaign.md):
//
//	envelope plan --phase A [--rtt 25,50] [--results FILE] [--commit C]   one line per cell to run: ID RTT LINK SHIFTED PROFILE
//	envelope profile --rtt N                                  the profile's settings, as shell assignments
//	envelope report --results FILE [--phase A|B]              the Markdown report
//	envelope publish --results FILE --data CSV                merges the valid rows into CSV, and its table into CSV's .md
package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/dihedron/veho/test/envelope"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "envelope:", err)
		os.Exit(1)
	}
}

func results(path string) ([]envelope.Row, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return envelope.ReadResults(f)
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: envelope plan|profile|report [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	phase := fs.String("phase", "", "A or B (report: both when empty)")
	rtts := fs.String("rtt", "", "round trips in ms, comma-separated (all when empty)")
	path := fs.String("results", "", "results.csv")
	commit := fs.String("commit", "", "plan: the nodes' build commit; cells done on another are run again")
	data := fs.String("data", "", "publish: the documentation's CSV; its table goes beside it, as .md")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	var rs []int
	for _, s := range strings.Split(*rtts, ",") {
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("--rtt %q: %w", s, err)
		}
		rs = append(rs, n)
	}
	switch args[0] {
	case "plan":
		rows, err := results(*path)
		if err != nil {
			return err
		}
		cells, err := envelope.Plan(*phase, rs, envelope.Done(rows, *commit))
		if err != nil {
			return err
		}
		for _, c := range cells {
			shifted := "-"
			if c.Jitter == "shift" {
				shifted = c.ShiftedLink()
			}
			fmt.Println(c.ID(), c.RTT, c.Link(), shifted, envelope.ProfileFor(c.RTT).Name)
		}
		fmt.Fprintf(os.Stderr, "%d cells, %v\n", len(cells), envelope.Duration(cells))
	case "profile":
		if len(rs) != 1 {
			return errors.New("profile: one --rtt")
		}
		p := envelope.ProfileFor(rs[0])
		fmt.Printf("name=%s etcd_heartbeat=%d etcd_election=%d ttl=%d loop_wait=%d retry_timeout=%d\n",
			p.Name, p.EtcdHeartbeat, p.EtcdElection, p.TTL, p.LoopWait, p.RetryTimeout)
	case "report":
		rows, err := results(*path)
		if err != nil {
			return err
		}
		var cells []envelope.Cell
		for _, ph := range []string{"A", "B"} {
			if *phase != "" && *phase != ph {
				continue
			}
			c, _ := envelope.Plan(ph, nil, nil)
			cells = append(cells, c...)
		}
		return envelope.Report(os.Stdout, rows, cells)
	case "publish":
		if *data == "" {
			return errors.New("publish: --data")
		}
		return publish(*path, *data)
	default:
		return fmt.Errorf("%q: want plan, profile, report or publish", args[0])
	}
	return nil
}

// publish merges results into the data CSV and rewrites its table.
func publish(path, data string) error {
	rows, err := results(path)
	if err != nil {
		return err
	}
	var old [][]string
	if b, err := os.ReadFile(data); err == nil { //nolint:gosec // the repository's own file
		recs, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
		if err != nil {
			return fmt.Errorf("%s: %w", data, err)
		}
		if len(recs) == 0 || strings.Join(recs[0], ",") != envelope.DataHeader {
			return fmt.Errorf("%s: no header, or another one", data)
		}
		old = recs[1:]
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	merged, err := envelope.Publish(old, rows)
	if err != nil {
		return err
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write(strings.Split(envelope.DataHeader, ","))
	_ = w.WriteAll(merged)
	if err := w.Error(); err != nil {
		return err
	}
	if err := os.WriteFile(data, []byte(b.String()), 0o644); err != nil { //nolint:gosec // documentation
		return err
	}
	table := strings.TrimSuffix(data, ".csv") + ".md"
	return os.WriteFile(table, []byte(envelope.Table(merged)), 0o644) //nolint:gosec // documentation
}
