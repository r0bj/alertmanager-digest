# alertmanager-digest

`alertmanager-digest` sends a Slack summary of active Alertmanager alerts.
It can also query Google Cloud Logging for alerts delivered to
`alertmanager-webhook-logger` during a configurable time window and include a
historical summary with occurrence counts.

## Historical alerts

Enable the `history` section in the config:

```yaml
history:
  enabled: true
  window: 24h
  projectIds:
  - bethink-prod
```

The tool uses Application Default Credentials and requires permission to call
Cloud Logging `entries.list` with the `logging.read` scope. The default log
filter is:

```text
resource.type="k8s_container" AND resource.labels.container_name="alertmanager-webhook-logger" AND jsonPayload.message="Events received"
```

The time window defaults to `24h` and can be overridden per run:

```sh
alertmanager-digest --config config.yaml --history-window 12h
```
