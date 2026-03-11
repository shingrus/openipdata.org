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

type GeofeedSummaryRow = {
  geofeed_count: number | string;
  last_success_at: Date | string | null;
};

type GeofeedSummary = {
  count: number;
  latestSuccessAt: string | null;
};

type GeofeedsQuerystring = {
  t?: string;
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

type GeofeedSummaryCacheEntry = {
  cachedAt: number;
  expiresAt: number;
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

function toGeofeedSummary(row: GeofeedSummaryRow | undefined): GeofeedSummary {
  if (!row) {
    return getEmptyGeofeedSummary();
  }

  const count = Number(row.geofeed_count);

  return {
    count: Number.isFinite(count) ? count : 0,
    latestSuccessAt: formatTimestamp(row.last_success_at)
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

async function loadGeofeedSummary(
  app: FastifyInstance,
  pool: Pool,
  queryTimeoutMs: number
): Promise<GeofeedSummary> {
  const queryStartedAt = process.hrtime.bigint();
  const query = {
    text: `
      select
        count(*) as geofeed_count,
        max(last_success_at) as last_success_at
      from geofeed_urls
    `
  };
  let result;

  try {
    result = await pool.query<GeofeedSummaryRow>(query);
  } catch (error) {
    const pgError = error as PgQueryError;

    app.log.error({
      err: error,
      query: {
        name: "select_geofeed_summary",
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

  return toGeofeedSummary(result.rows[0]);
}

export function registerGeofeedRoutes(app: FastifyInstance, options: RegisterGeofeedRoutesOptions): void {
  const cacheTtlMs = options.cacheTtlMs ?? defaultGeofeedCacheTtlMs;
  const queryLimit = options.queryLimit ?? defaultGeofeedLimit;
  let summaryCache: GeofeedSummaryCacheEntry | null = null;
  let inFlightSummaryLoad: Promise<GeofeedSummary> | null = null;

  async function getGeofeedSummary(): Promise<GeofeedSummary> {
    const now = Date.now();

    if (summaryCache && summaryCache.expiresAt > now) {
      return summaryCache.summary;
    }

    if (!options.pool) {
      summaryCache = null;
      return getEmptyGeofeedSummary();
    }

    if (!inFlightSummaryLoad) {
      inFlightSummaryLoad = loadGeofeedSummary(app, options.pool, options.queryTimeoutMs);
    }

    try {
      const summary = await inFlightSummaryLoad;
      const cachedAt = Date.now();

      summaryCache = {
        cachedAt,
        expiresAt: cachedAt + cacheTtlMs,
        summary
      };

      return summary;
    } catch (error) {
      if (summaryCache) {
        app.log.warn({
          err: error,
          cacheAgeMs: Math.max(0, Date.now() - summaryCache.cachedAt)
        }, "serving cached geofeed summary after refresh failure");

        return summaryCache.summary;
      }

      throw error;
    } finally {
      inFlightSummaryLoad = null;
    }
  }

  app.get("/geofeeds", async (_request, reply) => {
    const requestStartedAt = process.hrtime.bigint();
    let summary = getEmptyGeofeedSummary();

    try {
      summary = await getGeofeedSummary();
    } catch {
      summary = getEmptyGeofeedSummary();
    }

    reply.type("text/html; charset=utf-8");
    return reply.view("geofeeds.ejs", {
      csvUrl: "/api/geofeeds?t=csv",
      description: "All discovered geofeeds with the current total, the latest success check, and downloadable JSON or CSV exports.",
      geofeedCount: summary.count,
      jsonUrl: "/api/geofeeds?t=json",
      latestSuccessAtLabel: summary.latestSuccessAt ? formatTimestampLabel(summary.latestSuccessAt) : "No data",
      renderTime: formatElapsedMilliseconds(requestStartedAt),
      serviceName: options.serviceName,
      title: `All Discovered Geofeeds | ${options.serviceName}`
    });
  });

  app.get<{ Querystring: GeofeedsQuerystring }>("/api/geofeeds", async (request, reply) => {
    const format = request.query.t === "csv" ? "csv" : "json";

    try {
      if (!options.pool) {
        if (format === "csv") {
          reply.header("Content-Disposition", "attachment; filename=\"geofeeds.csv\"");
          return reply.type("text/csv; charset=utf-8").send(emptyGeofeedCsv);
        }

        if (request.query.t === "json") {
          reply.header("Content-Disposition", "attachment; filename=\"geofeeds.json\"");
        }

        return [];
      }

      const geofeeds = await loadGeofeeds(app, options.pool, queryLimit, options.queryTimeoutMs);

      if (format === "csv") {
        reply.header("Content-Disposition", "attachment; filename=\"geofeeds.csv\"");
        return reply.type("text/csv; charset=utf-8").send(toGeofeedCsv(geofeeds));
      }

      if (request.query.t === "json") {
        reply.header("Content-Disposition", "attachment; filename=\"geofeeds.json\"");
      }

      return geofeeds.map(toGeofeedApiRow);
    } catch {
      reply.code(503);

      if (format === "csv") {
        return reply.type("text/csv; charset=utf-8").send(emptyGeofeedCsv);
      }

      return [];
    }
  });
}
