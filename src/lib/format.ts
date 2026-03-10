export function formatElapsedMilliseconds(startedAt: bigint): string {
  const elapsedMs = Number(process.hrtime.bigint() - startedAt) / 1_000_000;
  return `${elapsedMs.toFixed(2)}ms`;
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
