import type { FastifyInstance } from "fastify";
import type { Pool } from "pg";
import { formatElapsedMilliseconds, formatTimestamp } from "../lib/format";

type GeofeedUrlRow = {
  url: string;
  last_checked_at: Date | string | null;
  last_fetch_status: string | null;
};

type GeofeedViewRow = {
  url: string;
  lastCheckedAt: string | null;
  result: string | null;
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

type RegisterGeofeedPageOptions = {
  pool: Pool | null;
  queryLimit?: number;
  queryTimeoutMs: number;
  serviceName: string;
};

const defaultGeofeedLimit = 15_000;

async function loadGeofeedUrls(
  app: FastifyInstance,
  pool: Pool,
  queryLimit: number,
  queryTimeoutMs: number
): Promise<GeofeedViewRow[]> {
  const queryStartedAt = process.hrtime.bigint();
  const query = {
    text: `
      select
        url,
        last_checked_at,
        last_fetch_status
      from geofeed_urls
      order by last_checked_at desc nulls last, url asc
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
    result: row.last_fetch_status,
    lastCheckedAt: formatTimestamp(row.last_checked_at),
    url: row.url
  }));
}

export function registerGeofeedPage(app: FastifyInstance, options: RegisterGeofeedPageOptions): void {
  const queryLimit = options.queryLimit ?? defaultGeofeedLimit;

  app.get("/geofeeds", async (_request, reply) => {
    const requestStartedAt = process.hrtime.bigint();
    let geofeeds: GeofeedViewRow[] = [];
    let loadError = false;

    if (options.pool) {
      try {
        geofeeds = await loadGeofeedUrls(app, options.pool, queryLimit, options.queryTimeoutMs);
      } catch {
        loadError = true;
      }
    }

    reply.type("text/html; charset=utf-8");
    return reply.view("geofeeds.ejs", {
      dbConfigured: Boolean(options.pool),
      description: "The most complete list of all discovered geofeeds, ordered by the most recent check time and fetch result.",
      geofeeds,
      loadError,
      renderTime: formatElapsedMilliseconds(requestStartedAt),
      serviceName: options.serviceName,
      title: `All Discovered Geofeeds | Most Complete Geofeed List | ${options.serviceName}`
    });
  });
}
