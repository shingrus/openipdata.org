import type { FastifyInstance } from "fastify";

type RegisterRobotsRouteOptions = {
  serviceName: string;
};

export function registerRobotsRoute(app: FastifyInstance, options: RegisterRobotsRouteOptions): void {
  const origin = `https://${options.serviceName}`;

  app.get("/robots.txt", async (_request, reply) => {
    const sitemapUrl = new URL("/sitemap.xml", origin).toString();
    const body = `User-agent: *\nAllow: /\nSitemap: ${sitemapUrl}\n`;

    reply.type("text/plain; charset=utf-8");
    return body;
  });
}
