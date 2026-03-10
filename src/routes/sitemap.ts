import type { FastifyInstance } from "fastify";

const sitemapPaths = ["/", "/docs", "/what-is-geofeed", "/what-is-asn", "/geofeeds"];

type RegisterSitemapRouteOptions = {
  serviceName: string;
};

function escapeXml(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("\"", "&quot;")
    .replaceAll("'", "&apos;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

export function registerSitemapRoute(app: FastifyInstance, options: RegisterSitemapRouteOptions): void {
  const origin = `https://${options.serviceName}`;

  app.get("/sitemap.xml", async (_request, reply) => {
    const urlEntries = sitemapPaths.map((pathname) => {
      const loc = escapeXml(new URL(pathname, origin).toString());
      return `  <url>\n    <loc>${loc}</loc>\n  </url>`;
    }).join("\n");
    const body = `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urlEntries}\n</urlset>\n`;

    reply.type("application/xml; charset=utf-8");
    return body;
  });
}
