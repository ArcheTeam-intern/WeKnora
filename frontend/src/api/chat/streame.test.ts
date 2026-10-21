import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { createServer, type ViteDevServer } from 'vite'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'

let server: ViteDevServer
let useStream: typeof import('./streame').useStream
let transport: { requests: Array<{ url: string; options: { body: string } }> }
const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
before(async () => {
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: {
    getItem: (key: string) => key === 'weknora_token' ? 'test-token' : null,
  } })
  const mocks: Record<string, string> = {
    '@microsoft/fetch-event-source': `export const requests = []; export async function fetchEventSource(url, options) { requests.push({url, options}); }`,
    '@/utils/index': `export const generateRandomString = () => 'test-request';`,
    '@/i18n': `export default { global: { t: key => key, locale: { value: 'zh-CN' } } };`,
  }
  server = await createServer({
    configFile: false,
    plugins: [{ name: 'stream-request-test', enforce: 'pre',
      resolveId(id) { if (id.startsWith('\0mock:')) return id },
      load(id) { if (id.startsWith('\0mock:')) return mocks[id.slice(6)] },
    }],
    optimizeDeps: { noDiscovery: true, entries: [] },
    resolve: { alias: [
      ...Object.keys(mocks).map(find => ({ find, replacement: '\0mock:' + find })),
      { find: '@', replacement: fileURLToPath(new URL('../../', import.meta.url)) },
    ] },
    server: { middlewareMode: true, hmr: false }, appType: 'custom',
  })
  ;({ useStream } = await server.ssrLoadModule('/src/api/chat/streame.ts'))
  transport = await server.ssrLoadModule('\0mock:@microsoft/fetch-event-source') as typeof transport
})
after(async () => {
  await server?.close()
  if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage)
  else Reflect.deleteProperty(globalThis, 'localStorage')
})

for (const effort of [undefined, '', 'off', 'auto', 'high', 'max']) {
  test(`SSE body carries only explicit reasoning overrides: ${effort}`, async () => {
    let stream!: ReturnType<typeof useStream>
    await renderToString(createSSRApp({ setup() { stream = useStream(); return () => null } }))
    try {
      await stream.startStream({
        session_id: 'session', query: 'question', method: 'POST', url: '/api/v1/agent-chat',
        reasoning_effort: effort,
      })
      assert.equal(stream.error.value, null)
      const body = JSON.parse(transport.requests.at(-1)!.options.body)
      assert.equal(body.reasoning_effort, effort || undefined)
      assert.equal(Object.hasOwn(body, 'reasoning_effort'), !!effort)
    } finally { stream.stopStream() }
  })
}
