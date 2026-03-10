# openipdata.org

Minimal `Fastify` + `TypeScript` SSR app for `openipdata.org`.

## What is included

- Server-rendered homepage at `/`
- JSON endpoint at `/api/health`
- Static assets served by the same Fastify app
- HTML templates stored in `templates/`
- `npm run dev` runs with `NODE_ENV=development`
- `npm start` runs with `NODE_ENV=production`
- Multi-stage `Dockerfile` for production builds
- GitHub Actions deploy flow that pushes to Docker Hub and updates a host-side Docker Compose app over SSH
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

## GitHub Actions deploy

The workflow at `.github/workflows/main.yml` replaces Fly.io deploys with this flow:

1. Build the Docker image for the pushed branch or manually selected ref
2. Push a `sha-<commit>` tag to Docker Hub and refresh `latest` on `main`
3. Copy `deploy/compose/compose.yaml` plus the current image tag metadata to `/opt/openipdata`
4. Run `docker compose pull && docker compose up -d` over SSH

Configure these repository settings before the first deploy:

- Repository variable: `DOCKERHUB_REPOSITORY` (example: `your-dockerhub-user/openipdata.org`)
- Repository secret: `DOCKERHUB_USERNAME`
- Repository secret: `DOCKERHUB_TOKEN`
- Repository secret: `SSH_HOST`
- Repository secret: `SSH_USER`
- Repository secret: `SSH_PRIVATE_KEY`

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
