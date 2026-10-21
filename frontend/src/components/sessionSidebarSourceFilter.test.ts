import assert from 'node:assert/strict'
import test from 'node:test'
import {
  DEFAULT_SESSION_BUCKET_KEY,
  buildSessionSourceOptions,
  findSessionBucketKey,
  shouldShowSessionSourceFilter,
} from './sessionSidebarSourceFilter.ts'

test('shouldShowSessionSourceFilter hides when no channel buckets', () => {
  assert.equal(shouldShowSessionSourceFilter(0), false)
  assert.equal(shouldShowSessionSourceFilter(1), true)
})

test('buildSessionSourceOptions puts web first then channels', () => {
  const options = buildSessionSourceOptions(
    'My chats',
    [{ key: 'api', label: 'API chats' }],
  )
  assert.equal(options.length, 2)
  assert.equal(options[0].value, DEFAULT_SESSION_BUCKET_KEY)
  assert.equal(options[1].value, 'api')
})

test('findSessionBucketKey locates session bucket', () => {
  const key = findSessionBucketKey(
    {
      web: { items: [{ id: 'a' }] },
      api: { items: [{ id: 'b' }] },
    },
    'b',
  )
  assert.equal(key, 'api')
})
