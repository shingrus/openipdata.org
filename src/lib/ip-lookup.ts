import type { IpLookupReader } from "./ip-lookup-reader";

type IpLookupLogger = {
  info?: (...args: unknown[]) => void;
  log?: (...args: unknown[]) => void;
};

type GeoRecord = {
  city?: {
    name?: string;
    names?: {
      en?: string;
    };
  };
  city_name?: string;
  country?: {
    iso_code?: string;
    names?: {
      en?: string;
    };
  };
  country_code?: string;
  country_name?: string;
  registered_country?: {
    iso_code?: string;
    names?: {
      en?: string;
    };
  };
};

type AsnRecord = {
  asn?: number | string;
  autonomous_system_number?: number;
  autonomous_system_organization?: string;
  name?: string;
  org_name?: string;
  organization?: string;
};

export type ClientIpReport = {
  asn?: string;
  cityName?: string;
  countryCode?: string;
  countryFlag?: string;
  countryName?: string;
  ip: string;
  orgName?: string;
};

type LookupClientIpReportOptions = {
  asnReader?: IpLookupReader | null;
  geoReader?: IpLookupReader | null;
  isDev?: boolean;
  logger?: IpLookupLogger;
};

const regionNames = typeof Intl.DisplayNames === "function"
  ? new Intl.DisplayNames(["en"], {
    type: "region"
  })
  : null;

export function lookupClientIpReport(
  ip: string,
  options: LookupClientIpReportOptions = {}
): ClientIpReport {
  const normalizedIp = ip.trim();
  const report: ClientIpReport = {
    ip: normalizedIp
  };
  const {
    asnReader,
    geoReader,
    isDev = false,
    logger = console
  } = options;

  if (!normalizedIp || !geoReader) {
    return report;
  }

  try {
    const geoRecord = geoReader.get(normalizedIp) as GeoRecord | null;

    if (!geoRecord) {
      return report;
    }

    const asnRecord = asnReader?.get(normalizedIp) as AsnRecord | null;

    if (isDev) {
      logInfo(logger, `Geo record: ${JSON.stringify(geoRecord)}`);

      if (asnRecord) {
        logInfo(logger, `ASN record: ${JSON.stringify(asnRecord)}`);
      }
    }

    const countryCode = pickString(
      geoRecord.country?.iso_code,
      geoRecord.registered_country?.iso_code,
      geoRecord.country_code
    );
    const countryName = pickString(
      geoRecord.country?.names?.en,
      geoRecord.registered_country?.names?.en,
      geoRecord.country_name,
      countryCodeToName(countryCode)
    );
    const cityName = pickString(
      geoRecord.city?.names?.en,
      geoRecord.city?.name,
      geoRecord.city_name
    );
    const orgName = pickString(
      asnRecord?.autonomous_system_organization,
      asnRecord?.organization,
      asnRecord?.org_name,
      asnRecord?.name
    );
    const asn = pickString(
      toStringValue(asnRecord?.autonomous_system_number),
      toStringValue(asnRecord?.asn)
    );

    if (countryCode) {
      report.countryCode = countryCode;
      report.countryFlag = countryCodeToFlag(countryCode);
    }

    if (countryName) {
      report.countryName = countryName;
    }

    if (cityName) {
      report.cityName = cityName;
    }

    if (orgName) {
      report.orgName = orgName;
    }

    if (asn) {
      report.asn = asn;
    }

    return report;
  } catch {
    return report;
  }
}

function countryCodeToFlag(countryCode: string | null | undefined): string | undefined {
  const normalizedCode = countryCode?.trim().toUpperCase() || "";

  if (!/^[A-Z]{2}$/.test(normalizedCode)) {
    return undefined;
  }

  return String.fromCodePoint(...[...normalizedCode].map((character) => character.charCodeAt(0) + 127_397));
}

function countryCodeToName(countryCode: string | null | undefined): string | undefined {
  const normalizedCode = countryCode?.trim().toUpperCase() || "";

  if (!regionNames || !/^[A-Z]{2}$/.test(normalizedCode)) {
    return undefined;
  }

  return regionNames.of(normalizedCode) || undefined;
}

function pickString(...values: Array<string | undefined | null>): string | undefined {
  for (const value of values) {
    if (typeof value === "string" && value.trim()) {
      return value.trim();
    }
  }

  return undefined;
}

function toStringValue(value: string | number | undefined): string | undefined {
  if (typeof value === "number" && Number.isFinite(value)) {
    return String(value);
  }

  return typeof value === "string" && value.trim()
    ? value.trim()
    : undefined;
}

function logInfo(logger: IpLookupLogger, message: string): void {
  if (typeof logger.info === "function") {
    logger.info(message);
    return;
  }

  if (typeof logger.log === "function") {
    logger.log(message);
  }
}
