import type { FastifyInstance } from "fastify";
import type { Pool } from "pg";
import { formatElapsedMilliseconds, formatTimestamp, formatTimestampLabel } from "../lib/format";

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

type GeofeedSummary = {
  count: number;
  latestSuccessAt: string | null;
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
  summary: GeofeedSummary;
};

const defaultGeofeedCacheTtlMs = 14_400_000;
const defaultGeofeedLimit = 15_000;
const emptyGeofeedCsv = "url,last_success_at\n";

function getEmptyGeofeedSummary(): GeofeedSummary {
  return {
    count: 0,
    latestSuccessAt: null
  };
}

function deriveSummary(geofeeds: GeofeedRecord[]): GeofeedSummary {
  if (geofeeds.length === 0) {
    return getEmptyGeofeedSummary();
  }

  // List is ordered by last_success_at desc nulls last, so first non-null is the latest
  const latestSuccessAt = geofeeds[0]?.lastSuccessAt ?? null;

  return {
    count: geofeeds.length,
    latestSuccessAt
  };
}

function escapeCsvValue(value: string): string {
  if (!/[",\r\n]/.test(value)) {
    return value;
  }

  return `"${value.replace(/"/g, "\"\"")}"`;
}

function toGeofeedApiRow(geofeed: GeofeedRecord): GeofeedApiRow {
  return {
    last_success_at: geofeed.lastSuccessAt,
    url: geofeed.url
  };
}

function toGeofeedCsv(geofeeds: GeofeedRecord[]): string {
  const rows = geofeeds.map((geofeed) => (
    `${escapeCsvValue(geofeed.url)},${escapeCsvValue(geofeed.lastSuccessAt ?? "")}`
  ));

  return rows.length > 0 ? `${emptyGeofeedCsv}${rows.join("\n")}` : emptyGeofeedCsv;
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
  let geofeedCache: GeofeedCacheEntry | null = null;
  let inFlightLoad: Promise<GeofeedRecord[]> | null = null;

  async function getGeofeedCache(): Promise<GeofeedCacheEntry> {
    const now = Date.now();

    if (geofeedCache && geofeedCache.expiresAt > now) {
      return geofeedCache;
    }

    if (!options.pool) {
      geofeedCache = null;
      return { cachedAt: now, expiresAt: now, geofeeds: [], summary: getEmptyGeofeedSummary() };
    }

    if (!inFlightLoad) {
      inFlightLoad = loadGeofeeds(app, options.pool, queryLimit, options.queryTimeoutMs);
    }

    try {
      const geofeeds = await inFlightLoad;
      const cachedAt = Date.now();

      geofeedCache = {
        cachedAt,
        expiresAt: cachedAt + cacheTtlMs,
        geofeeds,
        summary: deriveSummary(geofeeds)
      };

      return geofeedCache;
    } catch (error) {
      if (geofeedCache) {
        app.log.warn({
          err: error,
          cacheAgeMs: Math.max(0, Date.now() - geofeedCache.cachedAt)
        }, "serving cached geofeed data after refresh failure");

        return geofeedCache;
      }

      throw error;
    } finally {
      inFlightLoad = null;
    }
  }

  app.get("/geofeeds", async (_request, reply) => {
    const requestStartedAt = process.hrtime.bigint();
    let summary = getEmptyGeofeedSummary();

    try {
      const cache = await getGeofeedCache();
      summary = cache.summary;
    } catch {
      summary = getEmptyGeofeedSummary();
    }

    reply.type("text/html; charset=utf-8");
    return reply.view("geofeeds.ejs", {
      csvUrl: "/download/geofeeds.csv",
      description: "Public geofeed list and geofeed directory with all discovered geofeeds, public geofeed URLs, latest fetch times, and a JSON download.",
      geofeedCount: summary.count,
      jsonUrl: "/download/geofeeds.json",
      latestSuccessAtLabel: summary.latestSuccessAt ? formatTimestampLabel(summary.latestSuccessAt) : "No data",
      renderTime: formatElapsedMilliseconds(requestStartedAt),
      serviceName: options.serviceName,
      title: `All Discovered Geofeeds | Most Complete Geofeed List & Directory | ${options.serviceName}`
    });
  });

  app.get("/api/geofeeds", async (_request, reply) => {
    try {
      if (!options.pool) {
        return [];
      }

      const cache = await getGeofeedCache();
      return cache.geofeeds.map(toGeofeedApiRow);
    } catch {
      reply.code(503);
      return [];
    }
  });

  app.get("/download/geofeeds.csv", async (_request, reply) => {
    try {
      if (!options.pool) {
        reply.header("Content-Disposition", "attachment; filename=\"geofeeds.csv\"");
        return reply.type("text/csv; charset=utf-8").send(emptyGeofeedCsv);
      }

      const cache = await getGeofeedCache();
      reply.header("Content-Disposition", "attachment; filename=\"geofeeds.csv\"");
      return reply.type("text/csv; charset=utf-8").send(toGeofeedCsv(cache.geofeeds));
    } catch {
      reply.code(503);
      return reply.type("text/csv; charset=utf-8").send(emptyGeofeedCsv);
    }
  });

  app.get("/download/geofeeds.json", async (_request, reply) => {
    try {
      if (!options.pool) {
        reply.header("Content-Disposition", "attachment; filename=\"geofeeds.json\"");
        return [];
      }

      const cache = await getGeofeedCache();
      reply.header("Content-Disposition", "attachment; filename=\"geofeeds.json\"");
      return cache.geofeeds.map(toGeofeedApiRow);
    } catch {
      reply.code(503);
      return [];
    }
  });
}
