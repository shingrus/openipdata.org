# openipdata.org

Minimal `Fastify` + `TypeScript` SSR app for `openipdata.org`.

## What is included

- Server-rendered homepage at `/`
- Server-rendered geofeed list page at `/geofeeds`
- JSON endpoint at `/api/health`
- Static assets served by the same Fastify app
- HTML templates stored in `templates/`
- `npm run dev` runs with `NODE_ENV=development`
- `npm start` runs with `NODE_ENV=production`
- Multi-stage `Dockerfile` for production builds
- GitHub Actions deploy that pushes to Docker Hub and updates a host-side Docker Compose app over SSH on pushes to `main`
- Optional manual Fly.io GitHub Actions deploy
- nginx reverse-proxy config for `:80/:443 -> 127.0.0.1:9090`
- Docker Compose deployment example

## Local development

1. Install dependencies:

   ```bash
   npm install
   ```

2. Start the dev server:

   ```bash
   npm run dev
   ```

3. Open `http://127.0.0.1:9090`

Optional geofeed data:

- Set `PGSQL` to a PostgreSQL connection string.
- The `/geofeeds` page queries `geofeed_urls`.
- If `PGSQL` is unset, `/geofeeds` renders an empty state instead of failing.

## Production build

```bash
npm install
npm run build
NODE_ENV=production HOST=127.0.0.1 PORT=9090 npm start
```

## Deployment files

- Docker Compose app: `deploy/compose/compose.yaml`
- nginx config: `deploy/nginx/openipdata.org.conf`
- host path reference: `deploy/systemd/openipdata.service`
- environment example: `.env.example`

## GitHub Actions deploys

Default deploy:

- `.github/workflows/docker-host-deploy.yml`
- Trigger: push to `main`
- Deploy target: Docker Compose host over SSH
- Required secrets: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`, `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY`

Optional manual Fly deploy:

- `.github/workflows/main.yml`
- Trigger: `workflow_dispatch`
- Deploy target: Fly.io
- Required secret: `FLY_API_TOKEN`

Configure these repository settings for the default host deploy:

- Repository secret: `DOCKERHUB_USERNAME`
- Repository secret: `DOCKERHUB_TOKEN`
- Repository secret: `SSH_HOST`
- Repository secret: `SSH_USER`
- Repository secret: `SSH_PRIVATE_KEY`

The host deploy publishes to `DOCKERHUB_USERNAME/openipdata.org`, so no separate Docker Hub repository variable is needed.

Host-side requirements:

- Docker Engine and the Docker Compose plugin installed
- `/opt/openipdata` present and writable by the SSH user
- `/etc/openipdata/openipdata.env` present on the host
- The SSH user allowed to run `docker compose`
- nginx still proxies to `127.0.0.1:9090` on the host

The Compose file reads `/etc/openipdata/openipdata.env` for app-specific variables such as `SERVICE_NAME`. It deliberately overrides `HOST=0.0.0.0`, `PORT=9090`, and `NODE_ENV=production` inside the container so the app remains reachable through Docker's port mapping.

## Notes

- The host paths in the Docker Compose deploy match `deploy/systemd/openipdata.service`: `/opt/openipdata` for the app files and `/etc/openipdata/openipdata.env` for runtime configuration.
- The nginx config assumes Let's Encrypt certificates at `/etc/letsencrypt/live/openipdata.org/`.
