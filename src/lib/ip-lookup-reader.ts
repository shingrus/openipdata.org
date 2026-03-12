import { statSync } from "node:fs";
import { isIP } from "node:net";
import maxmind, { type Reader, type Response as MaxMindResponse } from "maxmind";

type JsonObject = Record<string, unknown>;

type IpLookupReaderLogger = {
  error: (...args: unknown[]) => void;
  info?: (...args: unknown[]) => void;
  log?: (...args: unknown[]) => void;
};

export type IpLookupReader = {
  get: (ip: string) => JsonObject | null;
  getLastUpdate: () => Date | null;
};

export type LoadIpLookupReaderOptions = {
  logger?: IpLookupReaderLogger;
  watchForUpdates?: boolean;
};

export async function loadIpLookupReader(
  databasePath: string | null | undefined,
  options: LoadIpLookupReaderOptions = {}
): Promise<IpLookupReader> {
  const logger = options.logger ?? console;
  const normalizedPath = databasePath?.trim() || null;
  const readerState: {
    lastUpdate: Date | null;
    reader: Reader<MaxMindResponse> | null;
  } = {
    lastUpdate: null,
    reader: null
  };

  if (!normalizedPath) {
    return {
      get: (): JsonObject | null => null,
      getLastUpdate: (): Date | null => null
    };
  }

  const markUpdated = (): void => {
    readerState.lastUpdate = getFileMtime(normalizedPath) ?? new Date();
  };

  try {
    readerState.reader = await maxmind.open(normalizedPath, {
      watchForUpdates: options.watchForUpdates ?? true,
      watchForUpdatesHook: () => {
        markUpdated();
        logInfo(logger, `Reloaded MMDB: ${normalizedPath}`);
      }
    });
    markUpdated();
  } catch (error) {
    logger.error(`Failed to load MMDB database: ${normalizedPath}`, error);
  }

  return {
    get: (ip: string): JsonObject | null => {
      const normalizedIp = ip.trim();

      if (!readerState.reader || !normalizedIp || !maxmind.validate(normalizedIp) || isPrivateIp(normalizedIp)) {
        return null;
      }

      try {
        const record = readerState.reader.get(normalizedIp);

        if (!record || Array.isArray(record) || typeof record !== "object") {
          return null;
        }

        return record as JsonObject;
      } catch {
        return null;
      }
    },
    getLastUpdate: (): Date | null => readerState.lastUpdate
  };
}

function logInfo(logger: IpLookupReaderLogger, message: string): void {
  if (typeof logger.info === "function") {
    logger.info(message);
    return;
  }

  if (typeof logger.log === "function") {
    logger.log(message);
  }
}

export function isPrivateIp(ip: string): boolean {
  const normalizedIp = ip.trim().toLowerCase();
  const family = isIP(normalizedIp);

  if (!normalizedIp || family === 0) {
    return false;
  }

  if (normalizedIp.startsWith("::ffff:")) {
    const mappedIpv4 = normalizedIp.slice(7);

    if (isIP(mappedIpv4) === 4) {
      return isPrivateIpv4(mappedIpv4);
    }
  }

  if (family === 4) {
    return isPrivateIpv4(normalizedIp);
  }

  return isPrivateIpv6(normalizedIp);
}

function getFileMtime(filePath: string): Date | null {
  try {
    return statSync(filePath).mtime;
  } catch {
    return null;
  }
}

function isPrivateIpv4(ip: string): boolean {
  const [first, second] = ip.split(".", 4).map((part) => Number.parseInt(part, 10));

  if (!Number.isInteger(first) || !Number.isInteger(second)) {
    return false;
  }

  return first === 0
    || first === 10
    || first === 127
    || (first === 100 && second >= 64 && second <= 127)
    || (first === 169 && second === 254)
    || (first === 172 && second >= 16 && second <= 31)
    || (first === 192 && second === 168)
    || (first === 198 && (second === 18 || second === 19))
    || first >= 224;
}

function isPrivateIpv6(ip: string): boolean {
  if (ip === "::" || ip === "::1") {
    return true;
  }

  const firstSegment = ip.split(":", 1)[0] || "0";
  const firstHextet = Number.parseInt(firstSegment, 16);

  if (Number.isNaN(firstHextet)) {
    return false;
  }

  return (firstHextet & 0xfe00) === 0xfc00
    || (firstHextet & 0xffc0) === 0xfe80
    || (firstHextet & 0xff00) === 0xff00;
}
