import { Pool } from "pg";

type CreateOptionalPostgresPoolOptions = {
  connectionString?: string | null;
  timeoutMs?: number;
};

export const defaultPostgresTimeoutMs = 20_000;

export function resolvePostgresConnectionString(env: NodeJS.ProcessEnv = process.env): string | null {
  const connectionString = env.PGSQL?.trim() || env.PGSQL_URL?.trim() || env.DATABASE_URL?.trim();
  return connectionString || null;
}

export function createOptionalPostgresPool(options: CreateOptionalPostgresPoolOptions = {}): Pool | null {
  const connectionString = options.connectionString ?? resolvePostgresConnectionString();

  if (!connectionString) {
    return null;
  }

  const timeoutMs = options.timeoutMs ?? defaultPostgresTimeoutMs;

  return new Pool({
    allowExitOnIdle: true,
    connectionString,
    connectionTimeoutMillis: timeoutMs,
    query_timeout: timeoutMs,
    statement_timeout: timeoutMs
  });
}

export async function closePostgresPool(pool: Pool | null): Promise<void> {
  if (pool) {
    await pool.end();
  }
}
