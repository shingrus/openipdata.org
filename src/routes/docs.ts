import type { FastifyInstance } from "fastify";
import { formatElapsedMilliseconds } from "../lib/format";

type RegisterDocsRoutesOptions = {
  serviceName: string;
};

export function registerDocsRoutes(app: FastifyInstance, options: RegisterDocsRoutesOptions): void {
  app.get("/docs", async (_request, reply) => {
    const requestStartedAt = process.hrtime.bigint();

    reply.type("text/html; charset=utf-8");
    return reply.view("docs.ejs", {
      description: "Documentation and explainers for openipdata.org, including what geofeeds are and how to use the geofeed directory.",
      renderTime: formatElapsedMilliseconds(requestStartedAt),
      serviceName: options.serviceName,
      title: `Docs | ${options.serviceName}`
    });
  });

  app.get("/what-is-geofeed", async (_request, reply) => {
    const requestStartedAt = process.hrtime.bigint();

    reply.type("text/html; charset=utf-8");
    return reply.view("what-is-geofeed.ejs", {
      description: "Learn what a geofeed is, see a simple example, read the RFC 8805 reference, and use the openipdata.org geofeed directory.",
      renderTime: formatElapsedMilliseconds(requestStartedAt),
      serviceName: options.serviceName,
      title: `What Is a Geofeed? | ${options.serviceName}`
    });
  });

  app.get("/what-is-asn", async (_request, reply) => {
    const requestStartedAt = process.hrtime.bigint();

    reply.type("text/html; charset=utf-8");
    return reply.view("what-is-asn.ejs", {
      description: "Learn what an ASN is, how Autonomous System Numbers are used in BGP, and why ASNs matter for Internet routing.",
      renderTime: formatElapsedMilliseconds(requestStartedAt),
      serviceName: options.serviceName,
      title: `What Is an ASN? | ${options.serviceName}`
    });
  });
}
