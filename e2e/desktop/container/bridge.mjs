// Forwards HTTP from 0.0.0.0:<listen> to Zotero's loopback-only connector
// server. The Host header is rewritten because Zotero's embedded httpd answers
// 400 unless the port in Host is the one it bound, and Docker's published
// port is a different number.
import http from 'node:http';

const [listenPort, targetPort] = process.argv.slice(2).map(Number);

http
  .createServer((req, res) => {
    const headers = { ...req.headers, host: `127.0.0.1:${targetPort}` };
    const upstream = http.request(
      { host: '127.0.0.1', port: targetPort, method: req.method, path: req.url, headers },
      (up) => {
        res.writeHead(up.statusCode ?? 502, up.headers);
        up.pipe(res);
      },
    );
    // Zotero may not be listening yet during startup; the harness retries.
    upstream.on('error', (e) => {
      res.writeHead(502, { 'content-type': 'text/plain' });
      res.end(`bridge: ${e.message}\n`);
    });
    req.pipe(upstream);
  })
  .listen(listenPort, '0.0.0.0');
