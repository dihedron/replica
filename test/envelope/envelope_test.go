package envelope

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// EN-T1: the cell plan: both phases, the profile of each round trip, the
// cells already in results.csv skipped, the plan's duration.
func TestPlan(t *testing.T) {
	a, err := Plan("A", nil, nil)
	if err != nil || len(a) != 30 {
		t.Fatalf("phase A: %d cells, %v", len(a), err)
	}
	b, err := Plan("B", nil, nil)
	if err != nil || len(b) != 18 {
		t.Fatalf("phase B: %d cells, %v", len(b), err)
	}
	ids := map[string]bool{}
	for _, c := range append(a, b...) {
		if ids[c.ID()] {
			t.Fatalf("two cells %s", c.ID())
		}
		ids[c.ID()] = true
	}
	row, err := Plan("A", []int{25, 50}, map[string]bool{"A-rtt25-j0": true})
	if err != nil || len(row) != 9 || row[0].ID() != "A-rtt25-j10" {
		t.Fatalf("two rows, one cell done: %v, %v", row, err)
	}
	if d := Duration(row); d != 9*time.Hour {
		t.Errorf("duration %v", d)
	}
	for _, bad := range [][]int{{30}, {0}} {
		if _, err := Plan("A", bad, nil); err == nil {
			t.Errorf("round trip %v accepted", bad)
		}
	}
	if _, err := Plan("C", nil, nil); err == nil {
		t.Error("phase C accepted")
	}
	for rtt, want := range map[int]string{25: "regional", 50: "regional", 100: "continental", 200: "wide", 400: "wide", 800: "extreme"} {
		if got := ProfileFor(rtt).Name; got != want {
			t.Errorf("profile of %d ms: %s, want %s", rtt, got, want)
		}
	}
}

// EN-T2: the netem settings of a cell and the profile's etcd and Patroni
// settings, from the envelope's §4 rules.
func TestSettings(t *testing.T) {
	for c, want := range map[Cell]string{
		{Phase: "A", RTT: 100, Jitter: "0"}:           "50/0/0",
		{Phase: "A", RTT: 100, Jitter: "25"}:          "50/12.5/0",
		{Phase: "A", RTT: 400, Jitter: "shift"}:       "200/0/0",
		{Phase: "B", RTT: 200, Jitter: "10", Loss: 1}: "100/10/1",
	} {
		if got := c.Link(); got != want {
			t.Errorf("%s: link %s, want %s", c.ID(), got, want)
		}
	}
	if s := (Cell{Phase: "A", RTT: 400, Jitter: "shift"}).ShiftedLink(); s != "260/0/0" {
		t.Errorf("shifted: %s", s)
	}
	for rtt, want := range map[int]Profile{
		25:  {Name: "regional", EtcdHeartbeat: 100, EtcdElection: 1000, TTL: 30, LoopWait: 10, RetryTimeout: 10},
		100: {Name: "continental", EtcdHeartbeat: 100, EtcdElection: 1000, TTL: 30, LoopWait: 10, RetryTimeout: 10},
		200: {Name: "wide", EtcdHeartbeat: 200, EtcdElection: 2000, TTL: 30, LoopWait: 10, RetryTimeout: 10},
		800: {Name: "extreme", EtcdHeartbeat: 800, EtcdElection: 8000, TTL: 30, LoopWait: 10, RetryTimeout: 10},
	} {
		if got := ProfileFor(rtt); got != want {
			t.Errorf("%d ms: %+v, want %+v", rtt, got, want)
		}
	}
	for _, rtt := range Phases["A"].RTTs {
		p := ProfileFor(rtt)
		switch {
		case p.EtcdElection < 10*p.EtcdHeartbeat || p.EtcdElection > 50000:
			t.Errorf("%d ms: election %d for heartbeat %d", rtt, p.EtcdElection, p.EtcdHeartbeat)
		case p.RetryTimeout*1000 < p.EtcdElection+2*rtt:
			t.Errorf("%d ms: retry_timeout %d s < E + 2R", rtt, p.RetryTimeout)
		case p.TTL < p.LoopWait+2*p.RetryTimeout:
			t.Errorf("%d ms: ttl %d < loop_wait + 2 x retry_timeout", rtt, p.TTL)
		}
	}
}

// EN-T5: the report from a fixed results.csv: the table, the claimed
// cells, the invalid and missing ones.
func TestReport(t *testing.T) {
	csv := Header + "\n" +
		"A-rtt25-j0,regional,true,0,0,12,40,35,70,40,0,1.2,0\n" +
		"A-rtt25-j50,regional,true,2,0,13,90,40,75,38,0,1.1,0\n" +
		"A-rtt800-j0,extreme,true,1,1,800,1900,310,90,10,2,1.4,0\n" +
		"A-rtt50-j0,regional,false,0,0,30,80,36,71,40,0,9.5,22\n"
	rows, err := ReadResults(strings.NewReader(csv))
	if err != nil || len(rows) != 4 {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	var out bytes.Buffer
	cells, _ := Plan("A", []int{25, 50, 800}, nil)
	if err := Report(&out, rows, cells); err != nil {
		t.Fatal(err)
	}
	r := out.String()
	for _, want := range []string{
		"| A-rtt25-j0 | regional | yes | 0 | 0 | 12 | 40 | 35 | 70 | pass |",
		"| A-rtt25-j50 | regional | no | 2 | 0 |",
		"| A-rtt800-j0 | extreme | yes | 1 | 1 | 800 | 1900 | 310 | 90 | fail |",
		"Invalid (run again): A-rtt50-j0",
		"Missing: A-rtt25-j10",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("the report lacks %q:\n%s", want, r)
		}
	}
	var w bytes.Buffer
	if err := WriteRow(&w, rows[0]); err != nil || strings.TrimSpace(w.String()) != strings.Split(csv, "\n")[1] {
		t.Fatalf("round trip of a row: %q, %v", w.String(), err)
	}
	if _, err := ReadResults(strings.NewReader("nonsense\n")); err == nil {
		t.Error("a file without the header")
	}
}

// The rows results.csv refuses, and the cells a run skips.
func TestResultsErrors(t *testing.T) {
	for _, body := range []string{
		Header + "\nA-rtt25-j0,regional,true\n",
		Header + "\nA-rtt25-j0,regional,true,x,0,12,40,35,70,40,0,1,0\n",
		Header + "\nA-rtt25-j0,regional,true,0,0,12,40,35,70,40,0,load,0\n",
		Header + "\n\"unterminated\n",
		"",
	} {
		if _, err := ReadResults(strings.NewReader(body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	done := Done([]Row{{Cell: "A-rtt25-j0", Valid: true}, {Cell: "A-rtt25-j10"}})
	if !done["A-rtt25-j0"] || done["A-rtt25-j10"] {
		t.Errorf("done: %v", done)
	}
}
