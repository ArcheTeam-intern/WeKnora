// Sidebar session list source filter: web chats default, optional API bucket.

export const DEFAULT_SESSION_BUCKET_KEY = 'web'

export interface SessionSourceOption {
  value: string
  label: string
}

export function shouldShowSessionSourceFilter(visibleChannelCount: number): boolean {
  return visibleChannelCount > 0
}

export function buildSessionSourceOptions(
  webLabel: string,
  channelBuckets: Array<{ key: string; label: string }>,
): SessionSourceOption[] {
  return [
    { value: DEFAULT_SESSION_BUCKET_KEY, label: webLabel },
    ...channelBuckets.map((bucket) => ({
      value: bucket.key,
      label: bucket.label,
    })),
  ]
}

export function findSessionBucketKey(
  buckets: Record<string, { items: Array<{ id: string }> }>,
  sessionId: string,
): string | null {
  for (const [key, bucket] of Object.entries(buckets)) {
    if (bucket.items.some((row) => row.id === sessionId)) return key
  }
  return null
}
