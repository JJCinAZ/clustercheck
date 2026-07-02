#!/bin/sh
# Usage: ./update.sh [IP_ADDRESS]
# If IP_ADDRESS is not provided, defaults to 192.168.91.11

IP=${1:-192.168.91.11}
echo "Building app for Linux/AMD64"
if ! GOOS=linux GOARCH=amd64 go build; then
    echo "Compile error"
    exit 1
fi

echo "Copying app to host at $IP"
if ! scp clustercheck $IP:/tmp/clustercheck; then
    echo "Copy error"
    exit 1
fi

echo "Updating and restarting clustercheck on host at $IP"
ssh -Tq $IP << EOF
sudo [ -f /opt/clustercheck/clustercheck ] && sudo mv -f /opt/clustercheck/clustercheck /opt/clustercheck/clustercheck.old
sudo install -m 0755 /tmp/clustercheck /opt/clustercheck/clustercheck
sudo setcap 'cap_net_bind_service=+ep' /opt/clustercheck/clustercheck
sudo systemctl restart clustercheck.service
EOF
