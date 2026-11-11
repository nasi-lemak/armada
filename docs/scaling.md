# Scaling and high availability (design)

Status: **design only, not implemented.** Armada today is one server process with SQLite and
local files. This note records what that handles, and what would change to run several replicas.

## What one server handles

- SQLite in WAL mode with a single writer. A heartbeat is a few small writes. At the default
  30-second heartbeat, 5,000 devices is about 170 heartbeats a second, which is comfortably within
  that budget on an SSD.
- Push connections are cheap: one idle HTTP request per device (a goroutine and a socket).
- Artifacts stream from disk with range support. Delta updates cut transfer volume a lot.
- Recovery: `armadactl backup` nightly; restore into a fresh data directory. Devices retry with
  backoff and catch up when the server returns. Nothing on a device depends on the server being up,
  except receiving new work.

For most fleets the right first step is a bigger machine and a tested restore, not replicas.

## Running several replicas

| Piece | Today | With replicas |
|---|---|---|
| Database | SQLite (`internal/store`) | Postgres. The store is plain SQL behind one package. The schema needs `BIGINT` timestamps and `ON CONFLICT` upserts, which both support. |
| Artifacts and chunk indexes | Local `artifacts/` directory | Object storage (S3-compatible). Devices fetch through the server, which keeps authorization, or through short-lived signed URLs. |
| Signing and license keys | Files in the data directory | Same keys on every replica, loaded from a secret store. |
| Rollout engine (`Tick`) | Runs in the process | Exactly one replica runs it, chosen with a Postgres advisory lock. Its work is idempotent, so a short overlap after failover is harmless. |
| Push wake-ups | In-memory hub | A device's long-poll lands on one replica. A wake is published with Postgres `LISTEN/NOTIFY`, and the replica holding the connection answers it. Pending wakes go in a table. |
| Sign-in throttling | In memory | A table or Redis, so limits hold across replicas. |
| Alert notifications | In-memory queue | A table used as a queue, drained by the engine leader. |
| History rollups | Engine tick | The same as the engine leader. |

Load balancing needs no sticky sessions: dashboard sessions and device keys are checked in the
database on every request. Long-polls should go to a load balancer with an idle timeout above
`-push-hold`.

## Order of work

1. Move the store to an interface with SQLite and Postgres implementations, and run the existing
   test suite against both.
2. Put artifact storage behind an interface (local directory or S3).
3. Add leader election for the engine, and `LISTEN/NOTIFY` for wake-ups.
4. Move throttling and the notification queue into the database.
