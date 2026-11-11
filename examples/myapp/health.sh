#!/bin/sh
# Example Armada health hook: the service must be running and answering.
# Armada retries this (health_retries x health_interval) before rolling back.
set -eu
systemctl is-active --quiet myapp
curl -fsS --max-time 5 http://127.0.0.1:8000/healthz >/dev/null
