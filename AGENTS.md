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

## API

- Keep the existing JSON surface minimal.
- Current endpoint: `/api/health`
- Do not introduce `/api/status` unless explicitly requested.

## Notes

- The dev script watches `templates/**/*.ejs`.
- Default bind is `127.0.0.1:9090` unless overridden by `HOST` or `PORT`.
- Prefer small, direct changes that preserve the current Fastify structure.
