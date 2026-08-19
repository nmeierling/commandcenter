const http = require('http');
const i = process.argv.indexOf('--port');
const port = i > -1 ? Number(process.argv[i + 1]) : 4000;
let ready = false;
setTimeout(() => { ready = true; console.log('api ready'); }, 800);
const server = http.createServer((req, res) => {
  if (req.url === '/health') {
    res.writeHead(ready ? 200 : 503, { 'content-type': 'application/json' });
    return res.end(JSON.stringify({ ok: ready }));
  }
  res.writeHead(200, { 'content-type': 'text/plain' });
  res.end('api ok\n');
});
server.listen(port, '127.0.0.1', () => console.log(`api listening on ${port}`));
let n = 0;
setInterval(() => console.log(`heartbeat ${++n}`), 1000);
process.on('SIGTERM', () => { console.log('api shutting down'); server.close(); process.exit(0); });
