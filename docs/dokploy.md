# Dokploy deployment settings

One service process owns each database schema. A replacement task that starts while the old task still runs exits with "another process owns this identity pool". Dokploy's defaults start the new task first, so deploy and roll back stop-first instead.

[`deploy/dokploy/swarm-settings.json`](../deploy/dokploy/swarm-settings.json) holds the application's Swarm settings. Durations are in nanoseconds, as Dokploy and Docker expect.

| Setting | Value | Reason |
| --- | --- | --- |
| Update and rollback order | `stop-first`, parallelism 1 | The old task releases ownership before the new one starts. |
| Update failure action | `rollback` | A replacement that fails its health check restores the previous version. |
| Rollback failure action | `pause` | A failed rollback waits for an operator instead of looping. |
| Update and rollback monitor | 60 s | A replacement that exits within a minute of becoming healthy counts as failed. One that never becomes healthy fails the update however long startup takes (see below). |
| Stop grace period | 60 s | Covers the 30-second request drain, return of cancelled identity operations, and the 10-second final save. |
| Health check | `/app/out healthcheck` every 10 s, 5 s timeout, 3 retries, 5-minute start period | Swarm marks the task running only once it is ready. Startup with a new pool registers identities before serving. |

The monitor period does not need to cover the 5-minute start period. In Docker 29.7.2, Swarm keeps a task in the starting state until its container reports healthy ([`Start`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/daemon/cluster/executor/container/controller.go#L256-L293), [agent](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/agent/exec/controller.go#L315-L321)). The update waits for each replacement to reach running ([`updateTask`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/update/updater.go#L424-L431)) before it starts the monitor period ([`Run`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/update/updater.go#L276-L289)). A replacement that fails before it is running counts as a failure whatever its age ([empty start time](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/update/updater.go#L371-L376), [`failureTriggersAction`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/update/updater.go#L217-L230)). In an isolated Swarm on Docker 29.7.2 with a 5-second monitor and a 40-second start period, a replacement that never became ready stayed starting and was rolled back after 50 seconds. A replacement that became ready after 25 seconds completed the update 5 seconds after it was running.

The health check path is the Railpack binary (`/app/out`). An image built from the `Dockerfile` already has its own `HEALTHCHECK`; to use it, clear the Dokploy health check or change its command to `tt-scrap healthcheck`.

Dokploy writes the whole service specification on every deploy, so settings changed with `docker service update` are lost at the next deploy. Dokploy stores these settings per application and applies them on each deploy. Set them under the application's **Advanced** tab (Health Check, Update Config, Rollback Config, Stop Grace Period), or with the API:

```sh
deploy/dokploy/swarm-settings.sh payload <applicationId> > payload.json
curl --fail -X POST "$DOKPLOY_URL/api/application.update" \
  -H "x-api-key: $DOKPLOY_API_KEY" -H 'Content-Type: application/json' --data @payload.json
```

Saving the settings does not restart the service. Apply them the first time with the procedure below; every deploy after that applies them by itself. A deploy stops the running task first, so the API is unavailable from the stop signal until the new task passes a health check. Each replacement gives the old task up to the 60-second stop grace period. With a complete saved identity pool, that outage is usually a few seconds when no requests are running. Startup and the first 10-second check interval come on top of the stop. This is an estimate, not a bound. A fresh or incomplete pool registers identities before serving, and slow proxies or upstream delays can extend startup through the 5-minute health check start period. A task that turns unhealthy (three failed checks after the start period) fails the update, and Swarm rolls back, which stops the new task and starts the previous version, adding another stop and startup to the outage.

### First deploy after saving the settings

Swarm copies the service specification into each task when it creates the task, and the task keeps that copy ([task model](https://github.com/moby/swarmkit/blob/master/design/task_model.md#spec)). Docker stops a task's container with the stop grace period from the task's own copy ([`shutdown`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/daemon/cluster/executor/container/adapter.go#L442-L446)). A task created before the settings were saved keeps Dokploy's default of 10 seconds. A normal deploy would stop it with that limit and kill the process during the request drain or the final save of identity state.

A rollback restores the previous specification and uses that specification's rollback settings ([`rollbackUpdate`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/update/updater.go#L592-L612), [`Run`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/update/updater.go#L160-L166)). After a normal first deploy those are Dokploy's `start-first` defaults, so the previous version would start while the new task still holds the identity pool.

Stopping the container by hand is not enough. With Dokploy's restart policy (Swarm's default: restart on any exit after 5 seconds), Swarm starts a replacement from the old specification ([`Restart`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/restart/restart.go#L149-L172)). The deploy then kills that replacement after 10 seconds. The procedure below pauses scheduling while the old task stops, then removes the replacement by scaling the service to zero. Scaling changes only the replica count, so it does not replace tasks ([`reconcile`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/replicated/services.go#L112-L121)). The deploy then sets the replica count back to Dokploy's value ([`Mode`](https://github.com/Dokploy/dokploy/blob/187de463dff6e4c7258d8b11c4c56c69e872044a/packages/server/src/utils/docker/utils.ts#L669-L677), [`service.update`](https://github.com/Dokploy/dokploy/blob/187de463dff6e4c7258d8b11c4c56c69e872044a/packages/server/src/utils/builders/index.ts#L181-L188)), and Swarm creates one task from the new specification. The procedure was tested on an isolated Docker 29.7.2 Swarm with a process that holds a PostgreSQL advisory lock and drains for 25 seconds.

Before the window, on the Docker host:

- `docker node ls` lists exactly one node, with availability `Active`. Pausing does not stop scheduling on other nodes.
- `docker service ls` shows every service at its desired replica count, and no other Dokploy deploy is running or queued. Turn off automatic deploys for the application until the procedure ends. A deploy that starts before step 6 stops the old task with the 10-second limit.
- The four settings are saved in Dokploy. `docker service inspect --format '{{.Spec.Mode.Replicated.Replicas}} {{if .UpdateStatus}}{{.UpdateStatus.State}}{{end}}' <appName>` prints `1`, followed by nothing, `completed` or `rollback_completed`.
- You can run SQL on the application's database (`DB_URL`).

Set `app=<appName>` and `node=$(docker node ls -q)`, then:

1. Pause scheduling: `docker node update --availability pause "$node"`. Running containers keep running. New tasks of every service on the host stay pending until step 5, so keep steps 1 to 5 short; they take about a minute and a half. If anything goes wrong before step 5, run step 5 before anything else.
2. Stop the old container with the full grace period. `ctr=$(docker ps -q -f label=com.docker.swarm.service.name="$app")` must be one container ID. Run `docker stop --time 60 "$ctr"`. `docker inspect --format '{{.State.ExitCode}}' "$ctr"` must print `0`; `137` means it was killed. `docker logs --tail 100 "$ctr"` must contain `service stopping` and must not contain `identity state not saved before shutdown`.
3. Wait until `docker service ps "$app"` shows the replacement with current state `Pending` and the error `no suitable node`. Swarm creates it about a second after the stop. Then run `docker service scale --detach "$app"=0`. Scaling before the replacement exists can leave it in the slot, and the deploy then replaces it as a rolling update.
4. Check that nothing of the service is left. `docker service ps "$app" --format '{{.DesiredState}}' | grep -vx -e Shutdown -e Remove` and `docker ps -q -f label=com.docker.swarm.service.name="$app"` must print nothing, both now and again 10 seconds later. The identity pool lock must be free. This query prints `0`; replace `public` with `DB_SCHEMA` if it is set:

   ```sql
   SELECT count(*) FROM pg_locks
   WHERE locktype = 'advisory' AND granted AND objsubid = 1
     AND classid = (hashtextextended('public.scrap_app.ram_pool', 0) >> 32)::bit(32)::int::oid
     AND objid = hashtextextended('public.scrap_app.ram_pool', 0)::bit(32)::int::oid;
   ```

5. Resume scheduling: `docker node update --availability active "$node"`. Repeat the checks from step 4; nothing may start.
6. Deploy the application in Dokploy. Swarm starts one task from the new specification. The service is down from step 2 until that task is healthy, including Dokploy's build.
7. `docker ps -f label=com.docker.swarm.service.name="$app" --format '{{.Status}}'` shows `(healthy)`, `docker node ls` shows `Active`, and the settings check below passes. Turn automatic deploys back on.

If the procedure stops before step 6, run step 5 if needed, then `docker service scale "$app"=1`. Swarm starts the previous version from the unchanged old specification. Do the same if Dokploy's build fails in step 6, because the service is still at zero replicas.

Swarm does not roll back the first deploy automatically. The deploy only adds a task, so there is no rolling update to roll back ([`Run`](https://github.com/moby/moby/blob/6a43e3d5afddf4111da0f864bbc7cae5d7e95001/vendor/github.com/moby/swarmkit/v2/manager/orchestrator/update/updater.go#L136-L150)). A new version that never becomes healthy is stopped and restarted by its restart policy, and the next attempt starts only after the previous container has exited. To stop it, run `docker service scale "$app"=0`; its task has the 60-second grace period. Then deploy a fixed or the previous commit from Dokploy. Do not run `docker service rollback` after this deploy: the previous specification is the one scaled to zero, so a rollback only stops the service. From the second deploy on, the previous specification carries the new settings, and an automatic rollback is `stop-first` with a 60-second grace period.

After the deploy, compare the live service with the settings:

```sh
docker service inspect <appName> | deploy/dokploy/swarm-settings.sh check
```

`check` exits 0 when the service matches, 1 with each differing field when it does not, and 2 when the input is not `docker service inspect` output for exactly one service. It needs `jq`, so run it where `jq` is installed and pipe the inspect output from the Docker host. `deploy/dokploy/swarm-settings-test.sh` tests it.

## Log retention

Dokploy 0.30 does not set a log driver for applications, and a per-service log option is lost at the next deploy. Bound container logs on the Docker host with [`deploy/dokploy/daemon.json`](../deploy/dokploy/daemon.json) (`json-file`, five 50 MB files per container) in `/etc/docker/daemon.json`. Docker reads these options only at daemon start and applies them to containers created afterwards. Swarm mode does not support live restore, so restarting the daemon stops every container on the host. Which containers come back, and in what order, depends on each one's Swarm service or restart policy. A container that is restarted rather than recreated keeps unbounded logs until it is next recreated, such as at its next deploy.
