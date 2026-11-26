# veho: the latency, jitter and loss campaign (AC-24)

This file specifies the lab campaign that measures the envelope of `veho-latency-envelope.md`: what inter-region round trip, jitter and packet loss the control plane supports, per profile, and how it degrades beyond. It turns that file's §3 estimates into measurements (LAT-1) and adds packet loss, which the envelope's sweep did not cover. It is part of `veho_specification.md`; the lab is `veho-lab.md`.

Status: approved, 2026-11-25, as proposed; the runs are overnight only (decision 8).

## 1. Why

`veho-latency-envelope.md` estimates, from etcd's, Patroni's and PostgreSQL's documented behaviour, how far each timeout profile stretches. Nothing has measured it. AC-18 (the wan24 campaign) checks one point, the reference deployment's latencies; AC-24 measures the whole envelope, so the Operator's Guide can say, from evidence, which profile to pick and where automatic cross-region failover stops being safe. Packet loss is added because a degraded link loses packets before it adds latency, and a lost heartbeat counts as a stall (envelope §3, jitter).

## 2. Decisions (approved 2026-11-25)

| # | Question | Decision |
| :--- | :--- | :--- |
| 1 | The cells | **Phase A, latency by jitter** (the envelope's §6): round trip 25, 50, 100, 200, 400 and 800 ms, by jitter none, ±10 %, ±25 %, ±50 % of the round trip, and a baseline shift (the round trip moves by 30 % at a random time, as a path change between sessions): 30 cells. **Phase B, latency by loss**: the same six round trips, at ±10 % jitter, by loss 0.1 %, 1 % and 3 % on every link: 18 cells. 48 cells of one hour. |
| 2 | The timeouts of a cell | The profile whose range holds the cell's round trip (`regional` up to 50 ms, `continental` to 150, `wide` to 400, `extreme` beyond), its etcd and Patroni settings from the envelope's §4 rules, applied before the cell by a new `lab.sh db profile NAME`: Patroni's dynamically (`patronictl edit-config`), etcd's by a rolling restart, one member at a time, waiting for a healthy cluster between them. A cell starts only after 5 minutes of quiet (no election) under the new settings; the settling is not measured. |
| 3 | An hour, split | Settle 5 min; **observe 45 min** of light load with nothing injected (the spurious elections and demotions, the acceptance latency); **inject 10 min**: kill the Patroni leader once (failover detection and recovery) and kill the veho node holding a running transfer once (adoption). The injections cause elections on purpose: they are counted apart, never as spurious. |
| 4 | The load | The soak's light load (one job at a time, no chaos), and a **prober** that submits a 1 MiB job with one destination every 15 s through a random node and records `CreateJob`'s latency, so p50 and p99 have about 180 samples per cell. |
| 5 | What is recorded | Every 10 s: etcd's leader and raft term, Patroni's leader and timeline. Per cell, one row of `results.csv`: the cell, the profile, spurious etcd elections, spurious Patroni demotions, acceptance p50 and p99, failover detection (the kill to the new primary's first accepted job), adoption time (the kill to another node's first progress on the transfer), jobs done and failed, the host's load and steal time. |
| 6 | Netem at large delays | The queue limit is raised (`limit 100000`) so 400 ms of one-way delay does not drop packets by itself; loss is random per packet; jitter is netem's normal distribution, with `rate` unset (the links are not bandwidth-limited in this campaign). |
| 7 | Validity | A cell whose host was saturated (load above the CPUs, or steal above 10 %) is marked invalid and run again: the lab, not the product, failed. A cell whose etcd or Patroni settings did not apply (read back after `db profile`) is not run. |
| 8 | Runs | 48 hours do not fit a night. The campaign takes the cells to run (`lab.sh campaign envelope --phase A --rtt 25,50`) and appends to the same `results.csv`, skipping the cells it holds; each run is planned to fit **one night** (AGENTS.md rule 14; decided 2026-11-25): a phase-A row is 5 hours, a phase-B row 3, so one or two rows a night and about a week in all. The hourly checks of AGENTS.md rule 15 apply to every run. |
| 9 | Pass | A profile **claims** the cells whose round trip is in its range and whose jitter is at most ±25 % with no loss; in every claimed cell: no spurious election or demotion, acceptance p99 under 1 s up to 150 ms (NFR-4), failover and recovery within 5 minutes (NFR-3). A cell outside the claims passes or fails without failing AC-24: it is documented with what happened. The report replaces the envelope's §3 table with the measured one and states, per profile, the jitter and loss it tolerated. |

## 3. The report

`lab.sh campaign envelope --report` turns `results.csv` into a Markdown table (one row per cell, the claimed cells marked) for `veho-latency-envelope.md` §3 and the Operator's Guide, and lists the invalid and the missing cells. The raw samples stay in the run's evidence directory.

## 4. Threat model

The campaign runs only in the lab and changes no product code; it adds no threat to the product. Its risks are to the evidence: **EV-1** a lab artefact read as a product failure (host saturation, a netem queue that drops, a container restarted by LXD): mitigated by decisions 6 and 7 and the per-cell host figures; **EV-2** an injected failure counted as spurious: mitigated by decision 3's separate window; **EV-3** settings that did not apply: mitigated by the read-back of decision 7.

## 5. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| EN-T1 | The cell plan: the phases, the profile of each round trip, the skip of cells already in `results.csv`, `--dry-run` printing the plan and its duration | unit (Go, the planner) |
| EN-T2 | The netem settings of a cell (delay, jitter, loss, limit) and the profile's etcd and Patroni settings, from the envelope's §4 rules | unit |
| EN-T3 | `lab.sh db profile NAME` applies and reads back each profile, the cluster healthy after the rolling restart | lab |
| EN-T4 | A smoke run of one cell shortened to 10 minutes (2 settle, 6 observe, 2 inject) writes a complete `results.csv` row with plausible figures | lab |
| EN-T5 | The report from a fixed `results.csv`: the table, the claimed cells, the invalid and missing ones | unit |

## 6. Out of scope

A chaos case with packet loss in the soak (1 to 5 % on one random link for 5 minutes, to check that jobs, leases and fencing ride out a degraded link): a separate backlog item (43). Bandwidth limits on the links. Asymmetric latency (one direction slower).
