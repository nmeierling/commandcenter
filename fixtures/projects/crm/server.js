const http = require('http');
// Honours PORT, like most dev servers. The agent sets it when you choose to
// start this project on a different port.
const port = Number(process.env.PORT) || 4001;
let ready = false;
setTimeout(() => { ready = true; console.log('crm ready'); }, 800);
const server = http.createServer((req, res) => {
  if (req.url === '/health') {
    res.writeHead(ready ? 200 : 503, { 'content-type': 'application/json' });
    return res.end(JSON.stringify({ ok: ready, port }));
  }
  res.writeHead(200, { 'content-type': 'text/plain' });
  res.end(`crm ok on ${port}\n`);
});
server.listen(port, '127.0.0.1', () => console.log(`crm listening on ${port}`));
let n = 0;
setInterval(() => console.log(`heartbeat ${++n}`), 1000);
process.on('SIGTERM', () => { console.log('crm shutting down'); server.close(); process.exit(0); });
