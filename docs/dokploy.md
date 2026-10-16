# Dokploy deployment settings

One service process owns each database schema. A replacement task that starts while the old task still runs exits with "another process owns this identity pool". Dokploy's defaults start the new task first, so deploy and roll back stop-first instead.

[`deploy/dokploy/swarm-settings.json`](../deploy/dokploy/swarm-settings.json) holds the application's Swarm settings. Durations are in nanoseconds, as Dokploy and Docker expect.

| Setting | Value | Reason |
| --- | --- | --- |
| Update and rollback order | `stop-first`, parallelism 1 | The old task releases ownership before the new one starts. |
| Update failure action | `rollback` | A replacement that fails its health check restores the previous version. |
| Rollback failure action | `pause` | A failed rollback waits for an operator instead of looping. |
| Update and rollback monitor | 60 s | A replacement that exits within a minute of becoming healthy counts as failed. |
| Stop grace period | 60 s | Covers the 30-second request drain, return of cancelled identity operations, and the 10-second final save. |
| Health check | `/app/out healthcheck` every 10 s, 5 s timeout, 3 retries, 5-minute start period | Swarm marks the task running only once it is ready. Startup with a new pool registers identities before serving. |

The health check path is the Railpack binary (`/app/out`). An image built from the `Dockerfile` already has its own `HEALTHCHECK`; to use it, clear the Dokploy health check or change its command to `tt-scrap healthcheck`.

Dokploy writes the whole service specification on every deploy, so settings changed with `docker service update` are lost at the next deploy. Dokploy stores these settings per application and applies them on each deploy. Set them under the application's **Advanced** tab (Health Check, Update Config, Rollback Config, Stop Grace Period), or with the API:

```sh
deploy/dokploy/swarm-settings.sh payload <applicationId> > payload.json
curl --fail -X POST "$DOKPLOY_URL/api/application.update" \
  -H "x-api-key: $DOKPLOY_API_KEY" -H 'Content-Type: application/json' --data @payload.json
```

Saving the settings does not restart the service; the next deploy applies them. Swarm uses the new update order for that deploy. The deploy stops the running task first, so the API is unavailable from the stop signal until the new task passes a health check. Once tasks created from these settings are running, each later replacement gives the old task up to the 60-second stop grace period. With a complete saved identity pool, that outage is usually a few seconds when no requests are running. Startup and the first 10-second check interval come on top of the stop. This is an estimate, not a bound. A fresh or incomplete pool registers identities before serving, and slow proxies or upstream delays can extend startup through the 5-minute health check start period. A task that turns unhealthy (three failed checks after the start period) fails the update, and Swarm rolls back, which stops the new task and starts the previous version, adding another stop and startup to the outage.

### First deploy after saving the settings

Swarm copies the service specification into each task when it creates the task, and the task keeps that copy ([task model](https://github.com/moby/swarmkit/blob/master/design/task_model.md#spec)). Docker stops a task's container with the stop grace period from the task's own copy ([`shutdown`](https://github.com/moby/moby/blob/master/daemon/cluster/executor/container/adapter.go)). A task created before the settings were saved keeps its old stop grace period. With Dokploy's defaults, that period is 10 seconds. The first deploy therefore stops that task with a 10-second limit, not 60 seconds. After 10 seconds Docker kills the process, which can cut off the request drain and the final save of identity state.

Rollback has the same limitation on the first deploy. Swarm restores the previous specification and then applies that specification's rollback settings. Dokploy's defaults use `start-first`, so a rollback of the first deploy starts the previous version while the new task still runs. The previous version then exits with "another process owns this identity pool" for as long as the new task runs. Watch the first deploy and resolve a rollback by hand.

During the first deploy, stop the old task's container yourself with an explicit 60-second timeout, such as `docker stop --time 60 <container>`, instead of relying on the 10-second limit. Swarm may start a replacement task from the current service specification once the container exits, depending on the service's restart policy. That task's copy still has the old 10-second limit. Decide and test the order of the controlled stop and the deploy before the maintenance window. Deploys after the first one need no extra step.

After the deploy, compare the live service with the settings:

```sh
docker service inspect <appName> | deploy/dokploy/swarm-settings.sh check
```

`check` exits 0 when the service matches, 1 with each differing field when it does not, and 2 when the input is not `docker service inspect` output for exactly one service. It needs `jq`, so run it where `jq` is installed and pipe the inspect output from the Docker host. `deploy/dokploy/swarm-settings-test.sh` tests it.

## Log retention

Dokploy 0.30 does not set a log driver for applications, and a per-service log option is lost at the next deploy. Bound container logs on the Docker host with [`deploy/dokploy/daemon.json`](../deploy/dokploy/daemon.json) (`json-file`, five 50 MB files per container) in `/etc/docker/daemon.json`. Docker reads these options only at daemon start and applies them to containers created afterwards. Swarm mode does not support live restore, so restarting the daemon stops every container on the host. Which containers come back, and in what order, depends on each one's Swarm service or restart policy. A container that is restarted rather than recreated keeps unbounded logs until it is next recreated, such as at its next deploy.
