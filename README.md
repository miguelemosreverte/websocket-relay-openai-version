WebSocket/UDP Relay with CI/CD, Benchmarks, and GitHub Pages

Overview
- Go server exposes a chat relay over WebSocket (primary) and UDP (experimental) with room and username encoded in URL path.
- Functional test doubles as a benchmark and exports console, JSON, and Markdown outputs.
- Renderer converts JSON benchmark results into a static HTML with charts.
- GitHub Actions deploys to Hetzner via SSH (git clone + systemd + Caddy/HTTPS), runs health checks, functional test, a 5s benchmark, and publishes results to GitHub Pages per-commit.

Endpoints
- `GET /health` — health check with version info
- `GET /ws/{room}/{username}` — WebSocket upgrade for a room; if `{room}` is omitted, `global` is used; `{username}` identifies the client

Configuration
- `PORT` (default: `8080`)
- `UDP_PORT` (default: `8081`)
- `ALLOWED_ORIGIN` (default: `*`)
- `DOMAIN` (for Caddy TLS via sslip.io)

Local Dev
- `go run .` to start server
- `go test -v -run TestFunctional -args -duration=2s` quick test
- `go test -v -run TestFunctional -args -duration=5s -report.json=out.json -report.md=out.md` benchmark mode
- `go run ./render.go -in out.json -out site/report.html` render HTML

CI/CD
- `ci.yml` builds and runs functional test
- `deploy.yml` deploys to Hetzner, validates health, runs functional test and benchmark, publishes Pages

Secrets to configure in GitHub
- `SSH_USER` (e.g., `root`)
- `SERVER_IP` (e.g., `95.217.238.72`)
- `SSH_PRIVATE_KEY` (private key for server)
- `DOMAIN` (e.g., `95-217-238-72.sslip.io`)

Note: Do NOT commit secrets to the repo. Add them as GitHub Actions secrets.
# websocket-relay-openai-version
