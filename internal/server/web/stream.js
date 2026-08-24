(function (root, factory) {
  const stream = factory()
  if (typeof module === 'object' && module.exports) module.exports = stream
  else root.graphStream = stream
})(globalThis, function () {
  async function read(reader, onEvent) {
    const decoder = new TextDecoder()
    let buffer = ''
    let result = null

    function consume(line) {
      if (!line.trim()) return
      const event = JSON.parse(line)
      if (onEvent) onEvent(event)
      if (event.type === 'error') throw new Error(event.error)
      if (event.type === 'result') result = event.result
    }

    while (true) {
      const {value, done} = await reader.read()
      buffer += decoder.decode(value || new Uint8Array(), {stream: !done})
      const lines = buffer.split('\n')
      buffer = lines.pop()
      for (const line of lines) consume(line)
      if (done) break
    }
    if (buffer.trim()) consume(buffer)
    if (!result) throw new Error('GitLab response ended before the graph was ready')
    return result
  }

  return {read}
})
