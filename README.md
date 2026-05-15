# alertmanager-digest

`alertmanager-digest` sends a Slack summary of active Alertmanager alerts.
It can also query Google Cloud Logging for alerts delivered to
`alertmanager-webhook-logger` during a configurable time window and include a
historical summary with occurrence counts.

## Slack webhook

Set `SLACK_WEBHOOK_URL` to the Slack incoming webhook URL before running the
digest:

```sh
export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/..."
alertmanager-digest --config config.yaml
```

The webhook can also be set as `slackWebhookUrl` in `config.yaml`, but the
environment variable is preferred for secrets.

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

Active and historical alerts are grouped by the top-level `groupBy` labels
before they are shown in Slack. When `groupBy` is omitted or empty, alerts are
grouped by all labels. Active alert counts are deduplicated by alert
`fingerprint`. Historical occurrence counts are deduplicated by alert
`fingerprint` and `startsAt` within each group, so repeated Alertmanager webhook
batches do not inflate the count for alerts that are still firing. Notification
counts show how many matching webhook alert entries were logged during the
window, and `last notified` is based on the latest matching Cloud Logging entry
timestamp.

The time window defaults to `24h` and can be overridden per run:

```sh
alertmanager-digest --config config.yaml --history-window 12h
```

## Empty digests

By default, Slack delivery is skipped when there are no active alerts, no
historical alerts, and no fetch errors. Set `slack.sendEmptyMessage` to `true`
to still send the all-clear digest:

```yaml
slack:
  sendEmptyMessage: true
```
