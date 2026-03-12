export function formatElapsedMilliseconds(startedAt: bigint): string {
  const elapsedMs = Number(process.hrtime.bigint() - startedAt) / 1_000_000;
  return `${elapsedMs.toFixed(2)}ms`;
}

const timestampLabelFormatter = new Intl.DateTimeFormat("en-US", {
  day: "numeric",
  hour: "2-digit",
  hour12: false,
  minute: "2-digit",
  month: "short",
  second: "2-digit",
  timeZone: "UTC",
  timeZoneName: "short",
  year: "numeric"
});

export type FormattedTimestamp = {
  iso: string;
  label: string;
};

export function formatTimestampPair(value: Date | string | null): FormattedTimestamp | null {
  const iso = formatTimestamp(value);
  const label = formatTimestampLabel(value);

  if (!iso || !label) {
    return null;
  }

  return { iso, label };
}

export function formatTimestamp(value: Date | string | null): string | null {
  if (!value) {
    return null;
  }

  const date = value instanceof Date ? value : new Date(value);

  if (Number.isNaN(date.getTime())) {
    return null;
  }

  return date.toISOString();
}

export function formatTimestampLabel(value: Date | string | null): string | null {
  if (!value) {
    return null;
  }

  const date = value instanceof Date ? value : new Date(value);

  if (Number.isNaN(date.getTime())) {
    return null;
  }

  return timestampLabelFormatter.format(date);
}
