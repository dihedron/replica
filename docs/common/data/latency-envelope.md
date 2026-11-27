Measured on veho 0.3.0-SNAPSHOT-0d84d64 (commit `0d84d64`), 2026-11-26.

| Cell | RTT (ms) | Jitter (%) | Loss (%) | Claimed | Spurious elections | Spurious demotions | Accept p50 (ms) | Accept p99 (ms) | Failover (s) | Adoption (s) | Jobs done | Verdict |
| :----------------- | ------: | -------: | -----: | :------ | ---------: | ---------: | ----------: | ----------: | --------: | --------: | -----: | :----------------- |
| A-rtt25-j0 | 25 | 0 | 0 | yes | 0 | 0 | 592 | 1350 | 38 | 57 | 576 | fail |
| A-rtt25-j10 | 25 | 10 | 0 | yes | 0 | 0 | 591 | 844 | 39 | 63 | 573 | pass |
| A-rtt25-j25 | 25 | 25 | 0 | yes | 0 | 0 | 584 | 849 | 42 | 54 | 501 | pass |
| A-rtt25-j50 | 25 | 50 | 0 | no | 0 | 0 | 583 | 727 | 49 | 66 | 215 | outside the claims |
| A-rtt25-jshift | 25 | shift | 0 | no | 0 | 0 | 593 | 815 | 42 | 61 | 539 | outside the claims |
| A-rtt50-j0 | 50 | 0 | 0 | yes | 0 | 0 | 1170 | 1269 | 42 | 65 | 365 | fail |
| A-rtt50-j10 | 50 | 10 | 0 | yes | 0 | 0 | 1186 | 1579 | 41 | 63 | 361 | fail |
| A-rtt50-j25 | 50 | 25 | 0 | yes | 0 | 0 | 1169 | 1335 | 39 | 63 | 306 | fail |
| A-rtt50-j50 | 50 | 50 | 0 | no | 0 | 0 | 1147 | 1641 | 30 | 64 | 154 | outside the claims |
| A-rtt50-jshift | 50 | shift | 0 | no | 0 | 0 | 1217 | 1594 | 28 | 59 | 376 | outside the claims |
