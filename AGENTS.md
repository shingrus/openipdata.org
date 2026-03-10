# AGENTS.md

## Overview

- App: `Fastify` + `TypeScript`
- Entry point: `src/server.ts`
- Static assets: `public/`
- Server-rendered views: `templates/`
- Deployment examples: `deploy/`

## Commands

- Install: `npm install`
- Dev server: `npm run dev`
- Build: `npm run build`
- Production start: `npm start`

## Rendering

- Do not add custom string-replacement templating.
- Server HTML should be rendered through `@fastify/view`.
- The current view engine is `EJS`.
- Homepage template: `templates/home.ejs`
- Shared footer partial: `templates/partials/footer.ejs`
- Render time belongs in the shared footer, not inside page content panels.
- Avoid artificial first-paint delays unless explicitly requested. Do not reintroduce page-load fade-ins or heavy blur effects by default.

## API

- Keep the existing JSON surface minimal.
- Current endpoints: `/api/health`, `/api/geofeeds`
- Do not introduce `/api/status` unless explicitly requested.

## Geofeeds

- Canonical page route: `/geofeeds`
- Current API route: `/api/geofeeds`
- Keep geofeed and Postgres-specific logic out of `src/server.ts`; prefer route modules under `src/routes/` and shared helpers under `src/lib/`.
- The `/geofeeds` page should load its data from `/api/geofeeds`; keep geofeed fetch/caching logic centralized on the server API path.
- Client-facing copy should refer to `Geofeeds` or `All Discovered Geofeeds`, not internal table names.
- Never expose internal limits, SQL details, or table names on the client side unless explicitly requested.
- Do not surface `geofeed_urls` in page text, headings, meta copy, or other client-visible HTML.
- The current `/api/geofeeds` JSON shape is an array of objects with `url` and `last_success_at`.
- Sort geofeeds only at the database level with `order by last_success_at desc nulls last`; do not add backend or client fallback sorting unless explicitly requested.
- The in-memory geofeeds cache currently defaults to 4 hours (`14_400_000` ms).
- The `JSON dump` link on `/geofeeds` should stay hidden until the API request succeeds.

## Environment

- Primary Postgres env variable: `PGSQL`
- `PGSQL_URL` and `DATABASE_URL` may be kept only as backward-compatible fallbacks unless explicitly removed.
- Never expose raw connection strings or secrets in templates or client-visible HTML; expose only safe derived booleans or labels if needed.

## Notes

- The dev script watches `templates/**/*.ejs`.
- Default bind is `127.0.0.1:9090` unless overridden by `HOST` or `PORT`.
- Prefer small, direct changes that preserve the current Fastify structure.
