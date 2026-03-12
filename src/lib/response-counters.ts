import type { FastifyBaseLogger } from "fastify";
import { createClient } from "redis";

export const responseCountersHashKey = "openipdata:response-counters";
export const defaultResponseCounterFlushIntervalMs = 5_000;

export enum ResponseCounterName {
  Api = "apicounter",
  Download = "downloadcounter",
  Root = "rootcounter"
}

export type ResponseCounterStore = {
  close: () => Promise<void>;
  flushCounterDeltas: (counterDeltas: ReadonlyMap<ResponseCounterName, number>) => Promise<void>;
  loadCounterValues: () => Promise<ResponseCounterValues | null>;
};

export type ResponseCounterBuffer = {
  incrementCounterFields: (fields: readonly ResponseCounterName[]) => void;
  stop: () => Promise<void>;
};

export type ResponseCounterValues = Record<ResponseCounterName, number>;

type CreateOptionalResponseCounterStoreOptions = {
  logger: FastifyBaseLogger;
  url?: string | null;
};

type CreateResponseCounterBufferOptions = {
  flushIntervalMs?: number;
  logger: FastifyBaseLogger;
  store: ResponseCounterStore | null;
};

export function createOptionalRedisResponseCounterStore(
  options: CreateOptionalResponseCounterStoreOptions
): ResponseCounterStore | null {
  const url = options.url?.trim() || null;

  if (!url) {
    return null;
  }

  const client = createClient({
    disableOfflineQueue: true,
    socket: {
      connectTimeout: 2_000
    },
    url
  });

  client.on("error", (error) => {
    options.logger.error({
      err: error
    }, "redis response counter client error");
  });

  void client.connect().catch((error) => {
    options.logger.error({
      err: error
    }, "redis response counter connection failed");
  });

  return {
    close: async (): Promise<void> => {
      if (!client.isOpen) {
        return;
      }

      await client.close();
    },
    flushCounterDeltas: async (counterDeltas: ReadonlyMap<ResponseCounterName, number>): Promise<void> => {
      if (!client.isReady || counterDeltas.size === 0) {
        return;
      }

      const pipeline = client.multi();

      for (const [field, delta] of counterDeltas) {
        pipeline.hIncrBy(responseCountersHashKey, field, delta);
      }

      await pipeline.execAsPipeline();
    },
    loadCounterValues: async (): Promise<ResponseCounterValues | null> => {
      if (!client.isReady) {
        return null;
      }

      const values = await client.hGetAll(responseCountersHashKey);

      return {
        [ResponseCounterName.Api]: toResponseCounterValue(values[ResponseCounterName.Api]),
        [ResponseCounterName.Download]: toResponseCounterValue(values[ResponseCounterName.Download]),
        [ResponseCounterName.Root]: toResponseCounterValue(values[ResponseCounterName.Root])
      };
    }
  };
}

export function createResponseCounterBuffer(
  options: CreateResponseCounterBufferOptions
): ResponseCounterBuffer | null {
  if (!options.store) {
    return null;
  }

  const flushIntervalMs = options.flushIntervalMs ?? defaultResponseCounterFlushIntervalMs;
  let counterDeltas = new Map<ResponseCounterName, number>();
  let flushPromise: Promise<void> | null = null;

  async function flushCounterDeltas(): Promise<void> {
    if (flushPromise) {
      await flushPromise;

      if (counterDeltas.size > 0) {
        await flushCounterDeltas();
      }

      return;
    }

    if (counterDeltas.size === 0) {
      return;
    }

    const nextCounterDeltas = counterDeltas;
    counterDeltas = new Map();
    flushPromise = (async () => {
      try {
        await options.store?.flushCounterDeltas(nextCounterDeltas);
      } catch (error) {
        options.logger.warn({
          err: error,
          counterDeltas: Object.fromEntries(nextCounterDeltas)
        }, "response counter flush failed");
      } finally {
        flushPromise = null;
      }
    })();

    await flushPromise;

    if (counterDeltas.size > 0) {
      await flushCounterDeltas();
    }
  }

  const flushTimer = setInterval(() => {
    void flushCounterDeltas();
  }, flushIntervalMs);
  flushTimer.unref();

  return {
    incrementCounterFields: (fields: readonly ResponseCounterName[]): void => {
      for (const field of dedupeCounterFields(fields)) {
        counterDeltas.set(field, (counterDeltas.get(field) ?? 0) + 1);
      }
    },
    stop: async (): Promise<void> => {
      clearInterval(flushTimer);
      await flushCounterDeltas();
    }
  };
}

function dedupeCounterFields(fields: readonly ResponseCounterName[]): ResponseCounterName[] {
  return [...new Set(fields)];
}

function toResponseCounterValue(value: string | undefined): number {
  const parsedValue = Number.parseInt(value ?? "", 10);

  return Number.isFinite(parsedValue) ? parsedValue : 0;
}
