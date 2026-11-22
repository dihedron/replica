# Inside one veho serve process

```mermaid
flowchart TB
  client(["Clients: veho CLI, applications"]) -->|"gRPC or REST, TLS 1.3, one port"| api
  subgraph node["veho serve"]
    api["API: authentication, roles, grants,<br/>rate limits, audit context"]
    engine["Engine: Submit, decide, Cancel, Retry"]
    wf["Workflows (DBOS):<br/>ReplicateJob, Transfer"]
    hk["Housekeeping:<br/>adoption, zombies, deadlines, retention"]
    hb["Heartbeat and fencing"]
    sec["Secrets: envelope encryption"]
    drv["Drivers: file, S3,<br/>process plugins"]
    api --> engine
    engine --> wf
    wf --> drv
    wf --> sec
    hk --> wf
  end
  engine -->|"one transaction:<br/>job, transfers, workflow"| pg[("PostgreSQL<br/>veho and dbos schemas")]
  wf --> pg
  hk --> pg
  hb --> pg
  sec --> pg
  drv -->|"parts"| st[["Storage systems"]]
  api -->|"events"| log[["System log, then SIEM"]]
