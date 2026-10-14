import assert from 'node:assert/strict';
import http from 'node:http';
import { once } from 'node:events';
import { fileURLToPath } from 'node:url';
import { createServer } from '../frontend/node_modules/vite/dist/node/index.js';

const seen = new Map();
const backend = http.createServer((req,res) => {
  const record={closed:false,completed:false};seen.set(req.url,record);
  const timer=setTimeout(()=>{record.completed=true;res.end('ok')},300);
  res.once('close',()=>{record.closed=true;clearTimeout(timer)});
});
backend.listen(0,'127.0.0.1');await once(backend,'listening');
process.env.PY_PROXY_TARGET=`http://127.0.0.1:${backend.address().port}`;
const root=fileURLToPath(new URL('../frontend',import.meta.url));
const vite=await createServer({root,configFile:`${root}/vite.config.ts`,logLevel:'silent',server:{port:0,host:'127.0.0.1',open:false}});
await vite.listen();
const port=vite.httpServer.address().port;
const wait=ms=>new Promise(r=>setTimeout(r,ms));
try {
  for(const path of ['/py/local/regions/xiongan/mosaic/16/1/1.png','/py/local/tiles/latest/16/1/1.png']){
    const req=http.get({hostname:'127.0.0.1',port,path});req.on('error',()=>{});
    for(let i=0;!seen.has(path)&&i<100;i++)await wait(10);
    assert.ok(seen.has(path),'upstream request started');
    req.destroy();await wait(100);
    assert.equal(seen.get(path).closed,true,'browser abort propagates to the tile backend');
    assert.equal(seen.get(path).completed,false,'backend response was still pending when cancelled');
  }
  const response=await fetch(`http://127.0.0.1:${port}/py/local/regions/xiongan/mosaic/16/2/2.png`);
  assert.equal(await response.text(),'ok');
  console.log(JSON.stringify({passed:true,cancelledPaths:2,normalResponse:true}));
} finally {
  await vite.close();backend.closeAllConnections();await new Promise(r=>backend.close(r));
}
