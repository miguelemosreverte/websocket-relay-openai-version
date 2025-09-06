#!/usr/bin/env bash
set -euo pipefail

# Deploys this repo on a Hetzner server using git clone + systemd + Caddy (Let's Encrypt via sslip.io)
# Required env: DOMAIN, SERVER_IP (optional, used for IP vhost), REPO_URL (defaults to origin), UDP_PORT, PORT

PORT="${PORT:-8080}"
UDP_PORT="${UDP_PORT:-8081}"
DOMAIN="${DOMAIN:-}"
SERVER_IP="${SERVER_IP:-}"
APP_DIR="/opt/websocket-relay"
BIN_NAME="websocket-relay"
SERVICE_NAME="websocket-relay.service"

echo "==> Deploying websocket-relay (PORT=$PORT UDP_PORT=$UDP_PORT DOMAIN=$DOMAIN)"

# Install prerequisites
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y git curl ca-certificates jq

# Install Go if missing (1.22)
if ! command -v go >/dev/null 2>&1; then
  echo "Installing Go..."
  curl -fsSL https://go.dev/dl/go1.22.5.linux-amd64.tar.gz -o /tmp/go.tar.gz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tar.gz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
fi

# Install Caddy if missing
if ! command -v caddy >/dev/null 2>&1; then
  echo "Installing Caddy..."
  apt-get install -y debian-keyring debian-archive-keyring apt-transport-https
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update && apt-get install -y caddy
fi

# Ensure app dir
mkdir -p "$APP_DIR"

# Clone or fetch latest
REPO_URL="${REPO_URL:-}"
if [ -z "$REPO_URL" ]; then
  if [ -d "/root/repo" ] && [ -d "/root/repo/.git" ]; then
    REPO_URL=$(git -C /root/repo remote get-url origin)
  else
    REPO_URL=$(git config --get remote.origin.url || echo "https://github.com/miguelemosreverte/websocket-relay-openai-version.git")
  fi
fi

if [ ! -d "$APP_DIR/.git" ]; then
  echo "Cloning $REPO_URL into $APP_DIR"
  git clone --depth=1 "$REPO_URL" "$APP_DIR"
else
  echo "Fetching latest in $APP_DIR"
  git -C "$APP_DIR" fetch --all --prune
  git -C "$APP_DIR" reset --hard origin/$(git -C "$APP_DIR" rev-parse --abbrev-ref HEAD)
fi

cd "$APP_DIR"
COMMIT_HASH=$(git rev-parse --short HEAD)
BUILD_TIME=$(date -u +"%Y-%m-%d %H:%M:%S UTC")
echo "Building commit $COMMIT_HASH"

GOFLAGS="-ldflags=-s -w -X main.CommitHash=$COMMIT_HASH -X 'main.BuildTime=$BUILD_TIME'"
go build $GOFLAGS -o "/usr/local/bin/$BIN_NAME" ./

# Systemd service
cat > "/etc/systemd/system/$SERVICE_NAME" <<EOF
[Unit]
Description=WebSocket/UDP Relay
After=network.target

[Service]
Type=simple
Environment=PORT=$PORT
Environment=UDP_PORT=$UDP_PORT
Environment=ALLOWED_ORIGIN=*
ExecStart=/usr/local/bin/$BIN_NAME
Restart=always
RestartSec=2
User=root
WorkingDirectory=$APP_DIR

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl restart "$SERVICE_NAME"

# Open firewall ports if UFW present
if command -v ufw >/dev/null 2>&1; then
  ufw allow 80/tcp || true
  ufw allow 443/tcp || true
  ufw allow ${UDP_PORT}/udp || true
fi

# Caddyfile for reverse proxy with Let's Encrypt
if [ -n "$DOMAIN" ]; then
  echo "Configuring Caddy for domain $DOMAIN"
  cat > /etc/caddy/Caddyfile <<EOF
$DOMAIN {
    reverse_proxy 127.0.0.1:$PORT
    @websocket {
        header Connection *Upgrade*
        header Upgrade websocket
    }
    reverse_proxy @websocket 127.0.0.1:$PORT
    encode gzip
    header {
        Access-Control-Allow-Origin "https://miguelemosreverte.github.io"
        Access-Control-Allow-Methods "GET, POST, OPTIONS"
        Access-Control-Allow-Headers "Content-Type"
        Access-Control-Max-Age "3600"
        X-Content-Type-Options nosniff
        X-Frame-Options DENY
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        -Server
    }
}

# Optional IP vhost with internal TLS
${SERVER_IP:-} {
    tls internal
    reverse_proxy 127.0.0.1:$PORT
}
EOF
  systemctl reload caddy || systemctl restart caddy || true
fi

echo "==> Health check"
code=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:$PORT/health || true)
if [ "$code" != "200" ]; then
  echo "Health check failed (HTTP $code)" >&2
  journalctl -u "$SERVICE_NAME" --no-pager -n 200 || true
  exit 1
fi
echo "Service healthy on :$PORT"

echo "==> Done: $COMMIT_HASH"
