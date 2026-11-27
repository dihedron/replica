Measured on veho 0.3.0-SNAPSHOT-afcc920 (commit `afcc920`), 2026-11-27.

| Cell | RTT (ms) | Jitter (%) | Loss (%) | Claimed | Spurious elections | Spurious demotions | Accept p50 (ms) | Accept p99 (ms) | Failover (s) | Adoption (s) | Jobs done | Verdict |
| :----------------- | ------: | -------: | -----: | :------ | ---------: | ---------: | ----------: | ----------: | --------: | --------: | -----: | :----------------- |
| A-rtt25-j0 | 25 | 0 | 0 | yes | 0 | 0 | 62 | 123 | 41 | 56 | 442 | pass |
| A-rtt25-j10 | 25 | 10 | 0 | yes | 0 | 0 | 63 | 120 | 27 | 59 | 450 | pass |
| A-rtt25-j25 | 25 | 25 | 0 | yes | 0 | 0 | 63 | 126 | 28 | 62 | 411 | pass |
| A-rtt25-j50 | 25 | 50 | 0 | no | 0 | 0 | 65 | 650 | 33 | 58 | 166 | outside the claims |
| A-rtt25-jshift | 25 | shift | 0 | no | 0 | 0 | 75 | 151 | 30 | 57 | 405 | outside the claims |
| A-rtt50-j0 | 50 | 0 | 0 | yes | 0 | 0 | 111 | 212 | 43 | 62 | 327 | pass |
| A-rtt50-j10 | 50 | 10 | 0 | yes | 0 | 0 | 111 | 178 | 33 | 56 | 331 | pass |
| A-rtt50-j25 | 50 | 25 | 0 | yes | 0 | 0 | 114 | 247 | 36 | 64 | 266 | pass |
| A-rtt50-j50 | 50 | 50 | 0 | no | 0 | 0 | 116 | 425 | 41 | 57 | 112 | outside the claims |
| A-rtt50-jshift | 50 | shift | 0 | no | 0 | 0 | 114 | 255 | 31 | 58 | 306 | outside the claims |
