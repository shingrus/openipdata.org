# openipdata.org

Minimal `Fastify` + `TypeScript` SSR app for `openipdata.org`.

## What is included

- Server-rendered homepage at `/`
- JSON endpoint at `/api/health`
- Static assets served by the same Fastify app
- HTML templates stored in `templates/`
- Template context includes explicit production/development environment flags
- `npm run dev` runs with `NODE_ENV=development`
- `npm start` runs with `NODE_ENV=production`
- nginx reverse-proxy config for `:80/:443 -> 127.0.0.1:9090`
- `systemd` service example

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

- nginx config: `deploy/nginx/openipdata.org.conf`
- systemd unit: `deploy/systemd/openipdata.service`
- environment example: `.env.example`

## Notes

- The provided `systemd` unit expects the app to live at `/srv/openipdata.org`.
- Adjust `User`, `Group`, and `WorkingDirectory` in the unit file to match your server.
- The nginx config assumes Let's Encrypt certificates at `/etc/letsencrypt/live/openipdata.org/`.
