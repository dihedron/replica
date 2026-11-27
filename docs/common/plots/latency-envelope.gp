# The latency envelope's charts (AC-24, .specs/veho-latency-campaign.md),
# from docs/common/data/latency-envelope.csv, drawn by docs/build.sh:
#
#   gnuplot -e "data='...'; byrtt='...'; out='DIR'; last='COMMIT'; version='V'; rtts='25 50';
#               jitters='0 10'; commits='C1 C2'; versions='V1 V2'" latency-envelope.gp
#
# data is the CSV in cell order (a round trip's jitters together); byrtt the
# same rows by commit, loss, jitter and round trip (a jitter's round trips
# together). gnuplot breaks a line at every row it skips, so each chart
# reads the order that keeps its series' rows contiguous.
#
# last and version: the commit the first two charts show, and its version;
# rtts and jitters: the round trips and jitters measured on it (no loss).
# commits and versions, in the same order: the series of the third chart,
# one per commit, oldest first.

set datafile separator ","
set terminal pdfcairo size 16cm,9cm font "DejaVu Sans,9" linewidth 1.5
set grid lc rgb "#d0d0d0"
set key outside right top box

# the columns, by number (the header names them)
c_commit = 1; c_rtt = 6; c_jitter = 7; c_loss = 8; c_p50 = 14; c_p99 = 15; c_done = 18

# pick(c, j, col): column col of a row of commit c with jitter j (a string) and
# no loss, NaN for the other rows.
pick(c, j, col) = (strcol(c_commit) eq c && strcol(c_jitter) eq j && column(c_loss) == 0) ? column(col) : NaN

# 1. acceptance by round trip and jitter, for the last commit
set output out . "/latency-envelope-acceptance.pdf"
set title "Job acceptance latency (CreateJob), veho " . version
set xlabel "Round trip between regions (ms)"
set ylabel "Latency (ms)"
set logscale x 2
set logscale y 10
set xtics (25, 50, 100, 200, 400, 800)
set yrange [100:*]
# NFR-4, drawn as far as it applies
nfr4(r) = r <= 150 ? 1000 : NaN
set samples 200
plot for [i = 1:words(jitters)] byrtt skip 1 using (pick(last, word(jitters, i), c_rtt)):(column(c_p99)) \
       with linespoints lt i pt 7 title "p99, jitter " . word(jitters, i) . " %", \
     for [i = 1:words(jitters)] byrtt skip 1 using (pick(last, word(jitters, i), c_rtt)):(column(c_p50)) \
       with linespoints lt i dt 3 pt 6 title "p50, jitter " . word(jitters, i) . " %", \
     nfr4(x) with lines dt 2 lc rgb "#c00000" title "NFR-4: p99 < 1 s"

# 2. jobs done in a cell by jitter and round trip, for the last commit
set output out . "/latency-envelope-throughput.pdf"
set title "Jobs done in a cell's hour, veho " . version
set xlabel "Jitter (% of the round trip)"
set ylabel "Jobs"
unset logscale
set xtics auto
set yrange [0:*]
y(c, r) = (strcol(c_commit) eq c && column(c_rtt) == r && column(c_loss) == 0 && strcol(c_jitter) ne "shift") ? column(c_jitter) : NaN
plot for [i = 1:words(rtts)] data skip 1 using (y(last, word(rtts, i) + 0)):(column(c_done)) \
       with linespoints lt i pt 7 title "round trip " . word(rtts, i) . " ms"

# 3. regressions: acceptance p99 by round trip, without jitter, per commit
set output out . "/latency-envelope-regression.pdf"
set title "Job acceptance p99 by version (no jitter)"
set xlabel "Round trip between regions (ms)"
set ylabel "Latency (ms)"
set logscale x 2
set logscale y 10
set xtics (25, 50, 100, 200, 400, 800)
set yrange [100:*]
plot for [i = 1:words(commits)] byrtt skip 1 using (pick(word(commits, i), "0", c_rtt)):(column(c_p99)) \
       with linespoints lt i pt 7 title word(versions, i), \
     nfr4(x) with lines dt 2 lc rgb "#c00000" title "NFR-4: p99 < 1 s"
