import assert from 'node:assert/strict'
import test from 'node:test'

import {
  applyBucketCountProbe,
  bucketVisible,
  buildBucketDefinitions,
  createEmptyBucket,
  prependSessionToWebBucket,
  type BucketDefinition,
} from './sessionSidebarBuckets.ts'

const apiDef: BucketDefinition = {
  key: 'api',
  apiSource: 'api',
  label: 'API',
  kind: 'api',
}

test('bucketVisible hides channel buckets until count is known', () => {
  const bucket = createEmptyBucket(apiDef)
  assert.equal(bucketVisible(bucket), false)
})

test('bucketVisible hides channel buckets with zero sessions after probe', () => {
  const bucket = applyBucketCountProbe(createEmptyBucket(apiDef), 0)
  assert.equal(bucketVisible(bucket), false)
})

test('bucketVisible shows channel buckets with sessions after probe', () => {
  const bucket = applyBucketCountProbe(createEmptyBucket(apiDef), 3)
  assert.equal(bucketVisible(bucket), true)
})

test('prependSessionToWebBucket inserts new session at front and bumps total', () => {
  const webDef: BucketDefinition = {
    key: 'web',
    apiSource: 'web',
    label: 'Chats',
    kind: 'web',
  }
  const bucket = { ...createEmptyBucket(webDef), total: 2, items: [{ id: 'a' }] }
  const next = prependSessionToWebBucket(bucket, { id: 'b', title: 'New' })
  assert.deepEqual(next.items.map((s) => s.id), ['b', 'a'])
  assert.equal(next.total, 3)
})

test('prependSessionToWebBucket is idempotent for existing session', () => {
  const webDef: BucketDefinition = {
    key: 'web',
    apiSource: 'web',
    label: 'Chats',
    kind: 'web',
  }
  const bucket = { ...createEmptyBucket(webDef), total: 1, items: [{ id: 'a' }] }
  const next = prependSessionToWebBucket(bucket, { id: 'a', title: 'Same' })
  assert.equal(next, bucket)
})

test('buildBucketDefinitions hides channel buckets unless admin', () => {
  const labels = { web: 'Chats', api: 'API' }
  const defs = buildBucketDefinitions(labels)
  assert.deepEqual(defs.map((def) => def.key), ['web'])

  const adminDefs = buildBucketDefinitions(labels, { includeAdminChannelBuckets: true })
  assert.deepEqual(
    adminDefs.map((def) => def.key),
    ['api', 'web'],
  )
})
