// Package envelope plans and reports the AC-24 campaign
// (.specs/veho-latency-campaign.md): the cells, the timeouts of each
// profile (veho-latency-envelope.md §4), the links' netem settings, the
// results.csv rows and the report.
package envelope

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CellDuration is one cell: 5 minutes of settling, 45 of observation, 10
// of injected failures (decision 3).
const CellDuration = time.Hour

// Phase is a set of cells: its round trips by its jitters (in % of the
// round trip, or "shift") and its losses (in %).
type Phase struct {
	RTTs    []int
	Jitters []string
	Losses  []float64
}

// Phases are A (latency by jitter) and B (latency by loss, at ±10 %).
var Phases = map[string]Phase{
	"A": {RTTs: []int{25, 50, 100, 200, 400, 800}, Jitters: []string{"0", "10", "25", "50", "shift"}, Losses: []float64{0}},
	"B": {RTTs: []int{25, 50, 100, 200, 400, 800}, Jitters: []string{"10"}, Losses: []float64{0.1, 1, 3}},
}

// Cell is one hour of the campaign.
type Cell struct {
	Phase  string
	RTT    int     // ms
	Jitter string  // % of the round trip, or "shift"
	Loss   float64 // %
}

// ID names the cell in results.csv: A-rtt100-j25, B-rtt100-l1.
func (c Cell) ID() string {
	if c.Phase == "B" {
		return fmt.Sprintf("B-rtt%d-l%s", c.RTT, num(c.Loss))
	}
	return fmt.Sprintf("A-rtt%d-j%s", c.RTT, c.Jitter)
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// Link is the netem setting of every link, DELAY/JITTER/LOSS in one-way
// ms, ms and %, as lab.sh wan set --link takes it: half the round trip
// each way, the jitter a share of it.
func (c Cell) Link() string {
	delay := float64(c.RTT) / 2
	jitter := 0.0
	if p, err := strconv.ParseFloat(c.Jitter, 64); err == nil {
		jitter = delay * p / 100
	}
	return num(delay) + "/" + num(jitter) + "/" + num(c.Loss)
}

// ShiftedLink is the link after the baseline shift of a "shift" cell: the
// round trip 30 % longer, as a path change between sessions.
func (c Cell) ShiftedLink() string {
	s := c
	s.RTT, s.Jitter = int(math.Round(float64(c.RTT)*1.3)), "0"
	return s.Link()
}

// Claimed: the profile claims the cell (decision 9): its round trip is in
// the profile's range (by construction), its jitter at most ±25 %, no loss.
func (c Cell) Claimed() bool {
	return c.Loss == 0 && slices.Contains([]string{"0", "10", "25"}, c.Jitter)
}

// Plan is the phase's cells for the round trips given (all when none), in
// order, without those done.
func Plan(phase string, rtts []int, done map[string]bool) ([]Cell, error) {
	p, ok := Phases[phase]
	if !ok {
		return nil, fmt.Errorf("phase %q: want A or B", phase)
	}
	if len(rtts) == 0 {
		rtts = p.RTTs
	}
	var out []Cell
	for _, r := range rtts {
		if !slices.Contains(p.RTTs, r) {
			return nil, fmt.Errorf("round trip %d ms: want one of %v", r, p.RTTs)
		}
		for _, j := range p.Jitters {
			for _, l := range p.Losses {
				c := Cell{Phase: phase, RTT: r, Jitter: j, Loss: l}
				if !done[c.ID()] {
					out = append(out, c)
				}
			}
		}
	}
	return out, nil
}

// Duration is how long the cells take.
func Duration(cells []Cell) time.Duration { return time.Duration(len(cells)) * CellDuration }

// Profile is the timeouts for a round trip (veho-latency-envelope.md §3,
// §4).
type Profile struct {
	Name                        string
	EtcdHeartbeat, EtcdElection int // ms
	TTL, LoopWait, RetryTimeout int // s
}

// ProfileFor applies §4's rules to the round trip R: heartbeat about R,
// never below 100 ms; election 10 heartbeats, at least 1 s, at most 50 s;
// retry_timeout at least E + 2R, never below 10 s; loop_wait 10 s; ttl at
// least loop_wait + 2 x retry_timeout, never below 30 s.
func ProfileFor(rtt int) Profile {
	p := Profile{Name: "extreme", LoopWait: 10}
	switch {
	case rtt <= 50:
		p.Name = "regional"
	case rtt <= 150:
		p.Name = "continental"
	case rtt <= 400:
		p.Name = "wide"
	}
	p.EtcdHeartbeat = max(100, rtt)
	p.EtcdElection = min(50000, max(1000, 10*p.EtcdHeartbeat))
	p.RetryTimeout = max(10, int(math.Ceil(float64(p.EtcdElection+2*rtt)/1000)))
	p.TTL = max(30, p.LoopWait+2*p.RetryTimeout)
	return p
}

// Row is one cell's line of results.csv.
type Row struct {
	Cell, Profile        string
	Valid                bool
	Elections, Demotions int     // spurious, in the observation
	P50, P99             int     // ms, CreateJob
	Failover, Adoption   int     // s
	JobsDone, JobsFailed int     //
	HostLoad, HostSteal  float64 // load per CPU, steal %
}

// Header is results.csv's first line.
const Header = "cell,profile,valid,spurious_elections,spurious_demotions,accept_p50_ms,accept_p99_ms,failover_s,adoption_s,jobs_done,jobs_failed,host_load,host_steal_pct"

// WriteRow appends a row.
func WriteRow(w io.Writer, r Row) error {
	cw := csv.NewWriter(w)
	err := cw.Write([]string{r.Cell, r.Profile, strconv.FormatBool(r.Valid), strconv.Itoa(r.Elections), strconv.Itoa(r.Demotions),
		strconv.Itoa(r.P50), strconv.Itoa(r.P99), strconv.Itoa(r.Failover), strconv.Itoa(r.Adoption),
		strconv.Itoa(r.JobsDone), strconv.Itoa(r.JobsFailed), num(r.HostLoad), num(r.HostSteal)})
	cw.Flush()
	return errors.Join(err, cw.Error())
}

// ReadResults reads results.csv.
func ReadResults(r io.Reader) ([]Row, error) {
	recs, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 || strings.Join(recs[0], ",") != Header {
		return nil, errors.New("results.csv: no header, or another one")
	}
	var rows []Row
	for i, f := range recs[1:] {
		if len(f) != 13 {
			return nil, fmt.Errorf("results.csv line %d: %d fields", i+2, len(f))
		}
		n := make([]int, 8)
		for k := range n {
			if n[k], err = strconv.Atoi(f[3+k]); err != nil {
				return nil, fmt.Errorf("results.csv line %d: %w", i+2, err)
			}
		}
		load, err1 := strconv.ParseFloat(f[11], 64)
		steal, err2 := strconv.ParseFloat(f[12], 64)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("results.csv line %d: %w", i+2, err)
		}
		rows = append(rows, Row{Cell: f[0], Profile: f[1], Valid: f[2] == "true", Elections: n[0], Demotions: n[1],
			P50: n[2], P99: n[3], Failover: n[4], Adoption: n[5], JobsDone: n[6], JobsFailed: n[7], HostLoad: load, HostSteal: steal})
	}
	return rows, nil
}

// Done is the valid cells of the rows, which a run skips.
func Done(rows []Row) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if r.Valid {
			out[r.Cell] = true
		}
	}
	return out
}

// Pass is decision 9's verdict on a claimed cell: no spurious election
// or demotion, acceptance p99 under 1 s up to 150 ms (NFR-4), failover
// within 5 minutes (NFR-3).
func Pass(c Cell, r Row) bool {
	return r.Elections == 0 && r.Demotions == 0 && (c.RTT > 150 || r.P99 < 1000) && r.Failover <= 300
}

// Report writes the Markdown table of the rows for the cells, then the
// invalid and the missing cells.
func Report(w io.Writer, rows []Row, cells []Cell) {
	by := map[string]Row{}
	for _, r := range rows {
		if r.Valid {
			by[r.Cell] = r
		}
	}
	fmt.Fprintln(w, "| Cell | Profile | Claimed | Spurious elections | Spurious demotions | Acceptance p50 (ms) | Acceptance p99 (ms) | Failover (s) | Adoption (s) | Verdict |")
	fmt.Fprintln(w, "| :--- | :--- | :--- | ---: | ---: | ---: | ---: | ---: | ---: | :--- |")
	var missing []string
	for _, c := range cells {
		r, ok := by[c.ID()]
		if !ok {
			missing = append(missing, c.ID())
			continue
		}
		claimed, verdict := "no", "outside the claims"
		if c.Claimed() {
			claimed, verdict = "yes", "fail"
			if Pass(c, r) {
				verdict = "pass"
			}
		}
		fmt.Fprintf(w, "| %s | %s | %s | %d | %d | %d | %d | %d | %d | %s |\n", c.ID(), r.Profile, claimed, r.Elections, r.Demotions,
			r.P50, r.P99, r.Failover, r.Adoption, verdict)
	}
	var invalid []string
	for _, r := range rows {
		if !r.Valid && !by[r.Cell].Valid {
			invalid = append(invalid, r.Cell)
		}
	}
	if len(invalid) > 0 {
		fmt.Fprintf(w, "\nInvalid (run again): %s\n", strings.Join(invalid, ", "))
	}
	if len(missing) > 0 {
		fmt.Fprintf(w, "\nMissing: %s\n", strings.Join(missing, ", "))
	}
}
