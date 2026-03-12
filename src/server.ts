import path from "node:path";
import fastify from "fastify";
import fastifyStatic from "@fastify/static";
import fastifyView from "@fastify/view";
import ejs from "ejs";
import { formatElapsedMilliseconds, formatTimestampPair } from "./lib/format";
import { lookupClientIpReport } from "./lib/ip-lookup";
import { loadIpLookupReader, type IpLookupReader } from "./lib/ip-lookup-reader";
import {
  closePostgresPool,
  createOptionalPostgresPool,
  defaultPostgresTimeoutMs
} from "./lib/postgres";
import {
  createResponseCounterBuffer,
  createOptionalRedisResponseCounterStore,
  defaultResponseCounterFlushIntervalMs,
  ResponseCounterName,
  type ResponseCounterBuffer,
  type ResponseCounterStore
} from "./lib/response-counters";
import { registerDocsRoutes } from "./routes/docs";
import { registerGeofeedRoutes } from "./routes/geofeeds";
import { registerRobotsRoute } from "./routes/robots";
import { registerSitemapRoute } from "./routes/sitemap";
import { registerSsRoute } from "./routes/ss";

type RuntimeStatus = {
  service: string;
  status: "ok";
  uptimeSeconds: number;
};

type DatabaseUpdateView = {
  label: string;
  iso: string;
  displayLabel: string;
};

const serviceName = process.env.SERVICE_NAME || "openipdata.org";
const defaultHost = "127.0.0.1";
const defaultPort = 9090;
const host = process.env.HOST || defaultHost;
const testIp = process.env.TEST_IP?.trim() || null;
const parsedPort = Number.parseInt(process.env.PORT || String(defaultPort), 10);
const port = Number.isNaN(parsedPort) ? defaultPort : parsedPort;
let isShuttingDown = false;
const appRootDir = path.join(__dirname, "..");
const templatesDir = path.join(__dirname, "..", "templates");
const postgresPool = createOptionalPostgresPool({
  timeoutMs: defaultPostgresTimeoutMs
});
let ip2asnReader: IpLookupReader | null = null;
let ip2geoReader: IpLookupReader | null = null;
let responseCounterBuffer: ResponseCounterBuffer | null = null;
let responseCounterStore: ResponseCounterStore | null = null;

const app = fastify({
  logger: true,
  trustProxy: true
});

app.register(fastifyStatic, {
  root: path.join(__dirname, "..", "public")
});
app.register(fastifyView, {
  root: templatesDir,
  engine: {
    ejs
  }
});
app.addHook("onResponse", (request, reply) => {
  if (request.is404 || reply.statusCode >= 500) {
    return;
  }

  const routeUrl = request.routeOptions.url;
  const counterNames: ResponseCounterName[] = [];

  if (routeUrl === "/") {
    counterNames.push(ResponseCounterName.Root);
  }

  if (routeUrl?.startsWith("/api/")) {
    counterNames.push(ResponseCounterName.Api);
  }

  if (routeUrl === "/api/geofeeds") {
    const query = request.query as { t?: string };

    if (query.t === "csv" || query.t === "json") {
      counterNames.push(ResponseCounterName.Download);
    }
  }

  if (counterNames.length > 0) {
    responseCounterBuffer?.incrementCounterFields(counterNames);
  }
});

function getRuntimeStatus(): RuntimeStatus {
  return {
    service: serviceName,
    status: "ok",
    uptimeSeconds: Math.round(process.uptime()),
  };
}

function getDatabaseUpdates(): DatabaseUpdateView[] {
  const entries: Array<{ label: string; date: Date | null }> = [
    { label: "IP to Geo", date: ip2geoReader?.getLastUpdate() ?? null },
    { label: "IP to ASN", date: ip2asnReader?.getLastUpdate() ?? null }
  ];

  return entries.flatMap(({ label, date }) => {
    const ts = formatTimestampPair(date);
    return ts ? [{ label, iso: ts.iso, displayLabel: ts.label }] : [];
  });
}

function getClientIpReport(ip: string) {
  return lookupClientIpReport(testIp || ip, {
    asnReader: ip2asnReader,
    geoReader: ip2geoReader,
    isDev: process.env.NODE_ENV !== "production",
    logger: app.log
  });
}

app.get("/", async (request, reply) => {
  const requestStartedAt = process.hrtime.bigint();
  const clientIpReport = getClientIpReport(request.ip);
  const databaseUpdates = getDatabaseUpdates();
  reply.type("text/html; charset=utf-8");
  return reply.view("home.ejs", {
    clientIpReportJson: JSON.stringify(clientIpReport, null, 2),
    databaseUpdates,
    description: "OpenIPdata.org homepage and service overview.",
    renderTime: formatElapsedMilliseconds(requestStartedAt),
    serviceName,
    title: serviceName,
    uptimeSeconds: Math.round(process.uptime())
  });
});

app.get("/api/health", async () => {
  return getRuntimeStatus();
});

app.get("/api/ip", async (request) => {
  return getClientIpReport(request.ip);
});

registerSitemapRoute(app, {
  serviceName
});
registerRobotsRoute(app, {
  serviceName
});
registerDocsRoutes(app, {
  serviceName
});
registerGeofeedRoutes(app, {
  pool: postgresPool,
  queryTimeoutMs: defaultPostgresTimeoutMs,
  serviceName
});
registerSsRoute(app, {
  getIp2asnReader: () => ip2asnReader,
  getIp2geoReader: () => ip2geoReader,
  loadResponseCounterValues: async () => await responseCounterStore?.loadCounterValues() ?? null,
  serviceName
});

async function start(): Promise<void> {
  try {
    responseCounterStore = createOptionalRedisResponseCounterStore({
      logger: app.log,
      url: process.env.REDIS_URL
    });
    responseCounterBuffer = createResponseCounterBuffer({
      flushIntervalMs: defaultResponseCounterFlushIntervalMs,
      logger: app.log,
      store: responseCounterStore
    });

    [ip2geoReader, ip2asnReader] = await Promise.all([
      loadIpLookupReader(process.env.IP2GEO_PATH || path.join(appRootDir, "ip2geo-latest.mmdb"), {
        logger: app.log
      }),
      loadIpLookupReader(process.env.IP2ASN_PATH || path.join(appRootDir, "ip2asn-latest.mmdb"), {
        logger: app.log
      })
    ]);

    await app.listen({
      host,
      port
    });
  } catch (error) {
    app.log.error(error);
    await responseCounterBuffer?.stop();
    await responseCounterStore?.close();
    await closePostgresPool(postgresPool);
    process.exit(1);
  }
}

async function shutdown(signal: string): Promise<void> {
  if (isShuttingDown) {
    return;
  }

  isShuttingDown = true;
  app.log.info({ signal }, "shutting down");
  await app.close();
  await responseCounterBuffer?.stop();
  await responseCounterStore?.close();
  await closePostgresPool(postgresPool);
  process.exit(0);
}

process.once("SIGINT", () => {
  void shutdown("SIGINT");
});

process.once("SIGTERM", () => {
  void shutdown("SIGTERM");
});

void start();
