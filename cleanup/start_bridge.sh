#!/bin/bash
# start_bridge.sh - The "One and Done" Plan 10 Launcher

# 1. Autodetect the plan9port Namespace
if [ -z "$NAMESPACE" ]; then
    # Look for an existing namespace
    NS_FOUND=$(ls -d /tmp/ns.$USER.* 2>/dev/null | head -n 1)
    if [ -z "$NS_FOUND" ]; then
        # Create a new one if none found
        export NAMESPACE=/tmp/ns.$USER.:o9
        mkdir -p "$NAMESPACE"
        echo "[Bridge] Created new Namespace: $NAMESPACE"
    else
        export NAMESPACE="$NS_FOUND"
        echo "[Bridge] Using existing Namespace: $NAMESPACE"
    fi
else
    echo "[Bridge] Using provided Namespace: $NAMESPACE"
fi

# 2. Check for Factotum
if ! 9p ls factotum >/dev/null 2>&1; then
    echo "[Bridge] Starting Factotum..."
    factotum -n
    sleep 1
fi

# 3. Launch the Synthetic Graphics Device (Background)
# This provides /dev/draw, /dev/mouse, etc.
if [ -f "./o9draw/drawsrv" ]; then
    echo "[Bridge] Starting o9 Synthetic Graphics..."
    ./o9draw/drawsrv &
else
    echo "[Warning] o9draw/drawsrv not found. Graphics will be disabled."
fi

# 4. Launch the Master Namespace Server (Wrapped in TLS)
# We use single quotes for the network address to prevent Bash expansion.
echo "[Bridge] o9 Master Node is online and waiting for RCPU connections..."
tlssrv -c ./rcpud -cert cert.pem -key key.pem 'tcp!*!17019'
