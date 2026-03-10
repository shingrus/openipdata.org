import type { FastifyInstance } from "fastify";
import type { Pool } from "pg";
import { formatElapsedMilliseconds, formatTimestamp } from "../lib/format";

type GeofeedUrlRow = {
  url: string;
  last_success_at: Date | string | null;
};

type GeofeedRecord = {
  lastSuccessAt: string | null;
  url: string;
};

type GeofeedApiRow = {
  last_success_at: string | null;
  url: string;
};

type PgQueryError = Error & {
  code?: string;
  constraint?: string;
  detail?: string;
  hint?: string;
  position?: string;
  routine?: string;
  schema?: string;
  severity?: string;
  table?: string;
};

type RegisterGeofeedRoutesOptions = {
  cacheTtlMs?: number;
  pool: Pool | null;
  queryLimit?: number;
  queryTimeoutMs: number;
  serviceName: string;
};

type GeofeedCacheEntry = {
  cachedAt: number;
  expiresAt: number;
  geofeeds: GeofeedRecord[];
};

const defaultGeofeedCacheTtlMs = 14_400_000;
const defaultGeofeedLimit = 15_000;

function toGeofeedApiRow(geofeed: GeofeedRecord): GeofeedApiRow {
  return {
    last_success_at: geofeed.lastSuccessAt,
    url: geofeed.url
  };
}

async function loadGeofeeds(
  app: FastifyInstance,
  pool: Pool,
  queryLimit: number,
  queryTimeoutMs: number
): Promise<GeofeedRecord[]> {
  const queryStartedAt = process.hrtime.bigint();
  const query = {
    text: `
      select
        url,
        last_success_at
      from geofeed_urls
      order by last_success_at desc nulls last
      limit $1
    `,
    values: [queryLimit]
  };
  let result;

  try {
    result = await pool.query<GeofeedUrlRow>(query);
  } catch (error) {
    const pgError = error as PgQueryError;

    app.log.error({
      err: error,
      query: {
        limit: queryLimit,
        name: "select_geofeed_urls",
        timeoutMs: queryTimeoutMs
      },
      elapsedMs: formatElapsedMilliseconds(queryStartedAt),
      pg: {
        code: pgError.code,
        constraint: pgError.constraint,
        detail: pgError.detail,
        hint: pgError.hint,
        position: pgError.position,
        routine: pgError.routine,
        schema: pgError.schema,
        severity: pgError.severity,
        table: pgError.table
      }
    }, "geofeed sql query failed");

    throw error;
  }

  return result.rows.map((row) => ({
    lastSuccessAt: formatTimestamp(row.last_success_at),
    url: row.url
  }));
}

export function registerGeofeedRoutes(app: FastifyInstance, options: RegisterGeofeedRoutesOptions): void {
  const cacheTtlMs = options.cacheTtlMs ?? defaultGeofeedCacheTtlMs;
  const queryLimit = options.queryLimit ?? defaultGeofeedLimit;
  let cache: GeofeedCacheEntry | null = null;
  let inFlightLoad: Promise<GeofeedRecord[]> | null = null;

  async function getGeofeeds(): Promise<GeofeedRecord[]> {
    const now = Date.now();

    if (cache && cache.expiresAt > now) {
      return cache.geofeeds;
    }

    if (!options.pool) {
      cache = null;
      return [];
    }

    if (!inFlightLoad) {
      inFlightLoad = loadGeofeeds(app, options.pool, queryLimit, options.queryTimeoutMs);
    }

    try {
      const geofeeds = await inFlightLoad;
      const cachedAt = Date.now();

      cache = {
        cachedAt,
        expiresAt: cachedAt + cacheTtlMs,
        geofeeds
      };

      return geofeeds;
    } catch (error) {
      if (cache) {
        app.log.warn({
          err: error,
          cacheAgeMs: Math.max(0, Date.now() - cache.cachedAt)
        }, "serving cached geofeeds after refresh failure");

        return cache.geofeeds;
      }

      throw error;
    } finally {
      inFlightLoad = null;
    }
  }

  app.get("/geofeeds", async (_request, reply) => {
    const requestStartedAt = process.hrtime.bigint();

    reply.type("text/html; charset=utf-8");
    return reply.view("geofeeds.ejs", {
      apiUrl: "/api/geofeeds",
      dbConfigured: Boolean(options.pool),
      description: "Public geofeed list and geofeed directory with all discovered geofeeds, public geofeed URLs, latest fetch times, and a JSON download.",
      renderTime: formatElapsedMilliseconds(requestStartedAt),
      serviceName: options.serviceName,
      title: `All Discovered Geofeeds | Most Complete Geofeed List & Directory | ${options.serviceName}`
    });
  });

  app.get("/api/geofeeds", async (_request, reply) => {
    try {
      return (await getGeofeeds()).map(toGeofeedApiRow);
    } catch {
      reply.code(503);
      return [];
    }
  });
}
