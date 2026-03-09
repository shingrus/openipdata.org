import path from "node:path";
import fastify from "fastify";
import fastifyStatic from "@fastify/static";
import fastifyView from "@fastify/view";
import ejs from "ejs";

type RuntimeStatus = {
  service: string;
  status: "ok";
  uptimeSeconds: number;
  startedAt: string;
};

const serviceName = process.env.SERVICE_NAME || "openipdata.org" ;
const defaultHost = "127.0.0.1";
const defaultPort = 9090;
const startedAt = new Date();
const host = process.env.HOST || defaultHost;
const parsedPort = Number.parseInt(process.env.PORT || String(defaultPort), 10);
const port = Number.isNaN(parsedPort) ? defaultPort : parsedPort;
let isShuttingDown = false;
const templatesDir = path.join(__dirname, "..", "templates");

const app = fastify({
  logger: true,
  trustProxy: true
});

void app.register(fastifyStatic, {
  root: path.join(__dirname, "..", "public")
});
void app.register(fastifyView, {
  root: templatesDir,
  engine: {
    ejs
  }
});

function getRuntimeStatus(): RuntimeStatus {
  return {
    service: serviceName,
    status: "ok",
    uptimeSeconds: Math.round(process.uptime()),
    startedAt: startedAt.toISOString()
  };
}

function formatElapsedMilliseconds(startedAt: bigint): string {
  const elapsedMs = Number(process.hrtime.bigint() - startedAt) / 1_000_000;
  return `${elapsedMs.toFixed(2)}ms`;
}

app.get("/", async (_request, reply) => {
  const requestStartedAt = process.hrtime.bigint();
  const status = getRuntimeStatus();
  reply.type("text/html; charset=utf-8");
  return reply.view("home.ejs", {
    renderTime: formatElapsedMilliseconds(requestStartedAt),
    serviceName: status.service,
    startedAt: status.startedAt,
    uptimeSeconds: status.uptimeSeconds
  });
});

app.get("/api/health", async () => {
  return getRuntimeStatus();
});


async function start(): Promise<void> {
  try {
    await app.listen({
      host,
      port
    });
  } catch (error) {
    app.log.error(error);
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
  process.exit(0);
}

process.on("SIGINT", () => {
  void shutdown("SIGINT");
});

process.on("SIGTERM", () => {
  void shutdown("SIGTERM");
});

void start();
