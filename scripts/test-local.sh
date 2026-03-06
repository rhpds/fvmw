#!/bin/bash
# Start/stop fvmw locally for testing.
# Usage:
#   ./scripts/test-local.sh start [esx]   # start in VPX or ESX mode
#   ./scripts/test-local.sh stop           # stop
#   ./scripts/test-local.sh status         # check status
#   ./scripts/test-local.sh test           # run govc tests

set -e
cd "$(dirname "$0")/.."

PORT=18443
PIDFILE=/tmp/fvmw-test.pid
DISK_PATH=/tmp/fvmw-test-disks

case "${1:-}" in
  start)
    # Kill any existing
    [ -f "$PIDFILE" ] && kill "$(cat $PIDFILE)" 2>/dev/null; rm -f "$PIDFILE"
    lsof -ti:$PORT | xargs kill -9 2>/dev/null || true

    # Create stub disks
    mkdir -p "$DISK_PATH"
    touch "$DISK_PATH"/{database,winweb01,winweb02,haproxy}.vmdk

    # Build
    go build -o /tmp/fvmw-test ./cmd/fvmw/

    # Set ESX mode if requested
    ESX_ENV=""
    if [ "${2:-}" = "esx" ]; then
      ESX_ENV="FVMW_ESX_MODE=1"
    fi

    # Start
    env FVMW_DISK_PATH="$DISK_PATH" FVMW_LISTEN_ADDR=":$PORT" \
        FVMW_USER_SUFFIX="-user1" $ESX_ENV \
        /tmp/fvmw-test -config config/default.yaml &
    echo $! > "$PIDFILE"
    sleep 2
    echo "fvmw started (pid=$(cat $PIDFILE), port=$PORT, mode=${2:-vpx})"
    ;;

  stop)
    if [ -f "$PIDFILE" ]; then
      kill "$(cat $PIDFILE)" 2>/dev/null && echo "stopped" || echo "not running"
      rm -f "$PIDFILE"
    else
      echo "not running"
    fi
    ;;

  status)
    if [ -f "$PIDFILE" ] && kill -0 "$(cat $PIDFILE)" 2>/dev/null; then
      echo "running (pid=$(cat $PIDFILE))"
    else
      echo "not running"
    fi
    ;;

  test)
    export GOVC_URL="http://administrator@vsphere.local:password@127.0.0.1:$PORT/sdk"
    export GOVC_INSECURE=1
    echo "=== About ===" && govc about
    echo "=== VMs ===" && govc find / -type m
    echo "=== apiType ===" && curl -s "http://127.0.0.1:$PORT/sdk" -H 'Content-Type: text/xml' \
      -d '<?xml version="1.0"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body><RetrieveServiceContent xmlns="urn:vim25"><_this type="ServiceInstance">ServiceInstance</_this></RetrieveServiceContent></soapenv:Body></soapenv:Envelope>' \
      | grep -o 'HostAgent\|VirtualCenter'
    ;;

  *)
    echo "Usage: $0 {start [esx]|stop|status|test}"
    exit 1
    ;;
esac
