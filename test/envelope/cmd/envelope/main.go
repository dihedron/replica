// Command envelope is the AC-24 campaign's planner and reporter, for
// test/lab/lib/envelope.sh (.specs/veho-latency-campaign.md):
//
//	envelope plan --phase A [--rtt 25,50] [--results FILE]   one line per cell to run: ID RTT LINK SHIFTED PROFILE
//	envelope profile --rtt N                                  the profile's settings, as shell assignments
//	envelope report --results FILE [--phase A|B]              the Markdown report
package main

import (
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
		cells, err := envelope.Plan(*phase, rs, envelope.Done(rows))
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
	default:
		return fmt.Errorf("%q: want plan, profile or report", args[0])
	}
	return nil
}
