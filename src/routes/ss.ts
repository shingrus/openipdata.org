import type { FastifyInstance } from "fastify";
import { formatElapsedMilliseconds, formatTimestampPair } from "../lib/format";
import type { IpLookupReader } from "../lib/ip-lookup-reader";
import { ResponseCounterName, type ResponseCounterValues } from "../lib/response-counters";

type RegisterSsRouteOptions = {
  getIp2asnReader: () => IpLookupReader | null;
  getIp2geoReader: () => IpLookupReader | null;
  loadResponseCounterValues: () => Promise<ResponseCounterValues | null>;
  serviceName: string;
};

type CounterRow = {
  label: string;
  value: number;
};

export function registerSsRoute(app: FastifyInstance, options: RegisterSsRouteOptions): void {
  app.get("/ss", async (_request, reply) => {
    const requestStartedAt = process.hrtime.bigint();
    const counterValues = await options.loadResponseCounterValues();
    const counterRows: CounterRow[] = [
      { label: "Home", value: counterValues?.[ResponseCounterName.Root] ?? 0 },
      { label: "API", value: counterValues?.[ResponseCounterName.Api] ?? 0 },
      { label: "Downloads", value: counterValues?.[ResponseCounterName.Download] ?? 0 }
    ];

    const geoTs = formatTimestampPair(options.getIp2geoReader()?.getLastUpdate() ?? null);
    const asnTs = formatTimestampPair(options.getIp2asnReader()?.getLastUpdate() ?? null);

    reply.type("text/html; charset=utf-8");
    return reply.view("ss.ejs", {
      asnTs,
      counterRows,
      countersAvailable: counterValues !== null,
      description: "Simple service counters for openipdata.org.",
      geoTs,
      renderTime: formatElapsedMilliseconds(requestStartedAt),
      serviceName: options.serviceName,
      title: `SS | ${options.serviceName}`
    });
  });
}
