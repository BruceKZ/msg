#!/bin/sh
# Build msg for the router and install/upgrade it over SSH.
# Usage: deploy/deploy.sh [ssh-host]   (default: openwrt)
set -eu
HOST=${1:-openwrt}
cd "$(dirname "$0")/.."

go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
	-ldflags "-s -w -X main.version=$(date +%Y%m%d-%H%M)" -o dist/msg-linux-arm64 .

# dropbear has no sftp-server, so stream files over ssh instead of scp.
ssh "$HOST" 'cat > /tmp/msg.new && chmod 755 /tmp/msg.new' < dist/msg-linux-arm64
ssh "$HOST" 'cat > /etc/init.d/msg && chmod 755 /etc/init.d/msg' < deploy/msg.init
ssh "$HOST" '
	set -e
	mkdir -p /etc/msg /mnt/ssd/msg
	chmod 700 /etc/msg
	if [ -f /etc/msg/config.json ]; then
		/tmp/msg.new -config /etc/msg/config.json check
	fi
	mv /tmp/msg.new /usr/bin/msg
	for f in /etc/msg/ /usr/bin/msg /etc/init.d/msg; do
		grep -qxF "$f" /etc/sysupgrade.conf || echo "$f" >> /etc/sysupgrade.conf
	done
	if [ ! -f /etc/msg/config.json ]; then echo "NOTE: create /etc/msg/config.json, then: /etc/init.d/msg enable && /etc/init.d/msg start"; exit 0; fi
	/etc/init.d/msg enable
	/etc/init.d/msg restart
	sleep 1
	msg version
	logread -e "msg\[" | tail -n 5
'
