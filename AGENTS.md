# AGENTS.md

## Overview

- App: `Fastify` + `TypeScript`
- Entry point: `src/server.ts`
- Route modules: `src/routes/`
- Shared helpers: `src/lib/`
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
- Geofeeds template: `templates/geofeeds.ejs`
- Shared footer partial: `templates/partials/footer.ejs`
- Keep MMDB lookups, timestamp formatting, and summary assembly in route/lib code; templates should only render prepared view data.
- Render time belongs in the shared footer, not inside page content panels.
- Avoid artificial first-paint delays unless explicitly requested. Do not reintroduce page-load fade-ins or heavy blur effects by default.

## Shared Libraries

- Keep reusable elapsed-time and timestamp formatting in `src/lib/format.ts`.
- Keep MMDB loading, reload tracking, and private-IP filtering in `src/lib/ip-lookup-reader.ts`.
- Keep homepage client IP report normalization in `src/lib/ip-lookup.ts`; do not render raw MaxMind records directly.
- Keep optional Postgres connection-string resolution and pool lifecycle in `src/lib/postgres.ts`.
- Prefer extending existing helpers in `src/lib/` over inlining duplicate formatting, env parsing, or lookup logic in routes.

## API

- Keep the existing JSON surface minimal.
- Current JSON endpoints: `/api/health`, `/api/ip`, `/api/geofeeds`
- `/api/ip` should return the same normalized client IP report object shown on the homepage for the current request IP.
- `/api/geofeeds?t=json` should keep the same JSON body and only add download headers.
- `/api/geofeeds?t=csv` is the CSV export form of the same geofeed dataset.
- Do not introduce `/api/status` unless explicitly requested.

## Geofeeds

- Canonical page route: `/geofeeds`
- Current API route: `/api/geofeeds`
- Keep geofeed and Postgres-specific logic out of `src/server.ts`; prefer route modules under `src/routes/` and shared helpers under `src/lib/`.
- Keep geofeed list queries and summary loading in `src/routes/geofeeds.ts` unless they become broadly reusable.
- The `/geofeeds` page currently renders its count/summary server-side and links to `/api/geofeeds` downloads; do not reintroduce client-side fetch/rendering unless explicitly requested.
- Client-facing copy should refer to `Geofeeds` or `All Discovered Geofeeds`, not internal table names.
- Never expose internal limits, SQL details, or table names on the client side unless explicitly requested.
- Do not surface `geofeed_urls` in page text, headings, meta copy, or other client-visible HTML.
- The current `/api/geofeeds` JSON shape is an array of objects with `url` and `last_success_at`.
- Sort geofeeds only at the database level with `order by last_success_at desc nulls last`; do not add backend or client fallback sorting unless explicitly requested.
- The in-memory geofeed summary cache currently defaults to 4 hours (`14_400_000` ms).
- On geofeed database failures, keep `/geofeeds` rendering with an empty summary and keep `/api/geofeeds` degrading to `[]` for JSON or header-only CSV with `503`.

## Homepage IP Data

- The homepage IP report should be derived through `lookupClientIpReport(...)`.
- Keep homepage and `/api/ip` responses aligned by sharing the same IP report preparation path.
- Expose normalized fields such as `ip`, `countryCode`, `countryFlag`, `countryName`, `cityName`, `orgName`, and `asn`; avoid leaking raw MMDB schema details into templates or client copy.
- Keep database update timestamps derived from `getLastUpdate()` and formatted through `formatTimestamp(...)` / `formatTimestampLabel(...)`.
- Missing MMDB files or unmapped/private IPs should degrade gracefully to partial or minimal report data rather than failing the page.

## Environment

- Primary Postgres env variable: `PGSQL`
- `PGSQL_URL` and `DATABASE_URL` may be kept only as backward-compatible fallbacks unless explicitly removed.
- Runtime MMDB env variables are `IP2GEO_PATH` and `IP2ASN_PATH`.
- If the MMDB env vars are unset, the app falls back to `ip2geo-latest.mmdb` and `ip2asn-latest.mmdb` at the app root.
- `TEST_IP` may override the detected client IP for homepage rendering.
- Never expose raw connection strings, filesystem paths, or secrets in templates or client-visible HTML; expose only safe derived booleans or labels if needed.

## Notes

- The dev script watches `templates/**/*.ejs`.
- Default bind is `127.0.0.1:9090` unless overridden by `HOST` or `PORT`.
- The Fastify app runs with `trustProxy: true`.
- Prefer small, direct changes that preserve the current Fastify structure.
