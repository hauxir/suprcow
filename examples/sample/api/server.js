// Tiny zero-dependency API. Serves a JSON greeting and a health endpoint.
import { createServer } from 'node:http'

const server = createServer((req, res) => {
  if (req.url === '/health') {
    res.writeHead(200, { 'content-type': 'text/plain' })
    res.end('ok')
    return
  }
  if (req.url.startsWith('/api/hello')) {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify({ message: 'Hello from the API 🐮', time: new Date().toISOString() }))
    return
  }
  res.writeHead(404, { 'content-type': 'text/plain' })
  res.end('not found')
})

server.listen(3000, () => console.log('api listening on :3000'))
