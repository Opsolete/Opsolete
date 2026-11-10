---
title: Operations
weight: 4
---

How the app runs once it is deployed: the work one replica does for all of them, the database connections each replica keeps, and what to monitor.

## Background work

Background work runs on one app replica at a time, the leader: failing jobs of lost runners (every 15 s), releasing orphaned state locks, queueing rollout batches and drift checks (every 15 s), queueing VCS-triggered runs and syncing the template catalog from Git (every minute), publishing registry module versions from VCS (every 5 minutes), and [retention]({{< relref "/docs/features/runs#retention" >}}): deleting expired run logs, drift checks and speculative plans, and expired sessions (hourly, at most 1000 runs per statement). The leader holds a PostgreSQL session advisory lock on a connection of its own; the other replicas try to take it every 5 seconds. When the leader stops or loses its database connection, Postgres releases the lock and another replica takes over within about 5 seconds (up to ~30 s if the leader's node vanishes without closing its connection); a job the old leader was running is cancelled. Every replica reloads the template catalog from Postgres every 10 seconds.

Every replica also keeps one more connection of its own, `LISTEN`ing for queued jobs and new log lines (application name `opsolete-notify`), so runner claims and live logs don't poll. If it breaks, the replica logs it and reconnects (backoff up to 30 s); meanwhile claims and log streams look every 5 seconds. Count two connections per replica beside the pool: this one and the leader lock's.

`opsolete_scheduler_leader` (see `METRICS_ADDR`) is 1 on the leader and 0 on the others: alert if `sum` isn't 1. The lock and `LISTEN` need a session: connect through a transaction-pooling proxy (PgBouncer `pool_mode=transaction`) and the leader isn't exclusive, and notifications are lost.
