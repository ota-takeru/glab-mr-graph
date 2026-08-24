const test = require('node:test')
const assert = require('node:assert/strict')
const {read} = require('../web/stream.js')

function chunkReader(chunks) {
  let index = 0
  return {
    async read() {
      if (index >= chunks.length) return {done: true}
      return {done: false, value: new TextEncoder().encode(chunks[index++])}
    },
  }
}

test('chunked NDJSON renders topology and returns the complete graph', async () => {
  const seen = []
  const result = await read(chunkReader([
    '{"type":"result","stage":"top',
    'ology","result":{"nodes":[{"id":"early"}],"edges":[]}}\n{"type":"res',
    'ult","stage":"complete","result":{"nodes":[{"id":"final"}],"edges":[]}}\n',
  ]), event => {
    if (event.type === 'result') seen.push([event.stage, event.result.nodes[0].id])
  })
  assert.deepEqual(seen, [['topology', 'early'], ['complete', 'final']])
  assert.equal(result.nodes[0].id, 'final')
})

test('stream error preserves the already delivered topology callback', async () => {
  const seen = []
  await assert.rejects(
    read(chunkReader([
      '{"type":"result","stage":"topology","result":{"nodes":[{"id":"early"}],"edges":[]}}\n',
      '{"type":"error","error":"status loading failed"}\n',
    ]), event => {
      if (event.type === 'result') seen.push(event.result.nodes[0].id)
    }),
    /status loading failed/,
  )
  assert.deepEqual(seen, ['early'])
})
