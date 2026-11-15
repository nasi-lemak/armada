# CLI

```
armadactl status
armadactl devices list [--channel C] [--label K=V] [--status online|offline] [--lifecycle spare|repair|retired|all]
armadactl devices get <id>
armadactl devices set <id> [--name N] [--channel C] [--label K=V]... [--unlabel K]...
                         [--asset-tag T] [--location L] [--contact C] [--notes N]
armadactl devices list --csv                 the devices, with serial and inventory, as CSV
armadactl devices action <id> reboot|restart-agent
armadactl devices action <id> exec -- systemctl status myapp
armadactl devices logs <id> [--unit myapp.service | --file myapp] [--lines 200] [--since 1h] [--grep error]
armadactl devices wake <id>
armadactl devices delete <id>
armadactl devices lifecycle <id> active|spare|repair [--note N]
armadactl devices retire <id> --yes [--note N]
armadactl devices replace <id> --with <new-id> [--old-state repair|spare|retired] [--note N]
armadactl tokens create|list|revoke             (create --spare: devices join as spares)
armadactl releases upload|list|delete
armadactl deployments create --release <id> [--channel C] [--label K=V] [--device ID]
                             [--stages 5,25,100] [--stage-duration 15m] [--max-failure-rate 0.2]
                             [--min-samples 3] [--manual] [--allow-downgrade]
armadactl deployments list|get|approve|pause|resume|advance|retry|complete|abort
armadactl events [--device ID] [--deployment ID] [--type T,prefix.*] [--actor WHO|system]
                 [--search TEXT] [--since 2026-11-01] [--until 2026-11-30] [--limit N] [--csv]
armadactl alerts [--firing] | alerts ack <id> [--note N] | alerts unack <id>
armadactl bulk reboot|restart-agent|wake|netcheck|lock|exec|label|unlabel|set-channel|lifecycle <devices> [--yes]
armadactl bulk list | bulk get <id>
armadactl report [--label K=V]... [--period last-month] [--from D --to D] [--tz TZ] [--format csv|html -o FILE]
armadactl report schedules list|create|send|delete
armadactl certs list|renew|csrs|email|upload|cancel
armadactl certs policies list|create|enable|disable|run|delete
armadactl maintenance list|start|schedule|end|delete   (start --device ID --for 2h; schedule --windows 02:00-04:00 --days sun)
armadactl config list|get|apply -f FILE|enable|disable|delete
armadactl groups [list|map --group G --role R [--scope K=V]...|unmap G]
armadactl scim tokens [list|create --name N|revoke ID]
armadactl whoami
armadactl users list|create|set|delete      (create --sso-only; set --grant ROLE[:K=V], --reset-2fa, --unlink-sso)
armadactl me password
armadactl me tokens [list|create --name N|revoke ID]
```

Authenticate with `--token` or `ARMADA_TOKEN`: a personal token, or the admin token.

Add `--json` to any command for machine-readable output.

---

[← Armada documentation](../README.md#documentation)
