// Exercise real Leaflet lifecycle and observed requests, without touching imagery.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const url = process.env.FRONTEND_URL || 'http://localhost:5173';
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const page = await browser.newPage();
const errors = []; page.on('pageerror', e => errors.push(e.message));
const ready = () => page.waitForFunction(() => {
  const h = window.h, layer = h.cache.active?.layer;
  if (!layer || !layer.getContainer() || !h.cache.scheduler.ready) return false;
  const tiles = Object.values(layer._tiles).filter(t => t.current);
  return tiles.length && tiles.every(t => t.el.naturalWidth === 256) && !layer.isLoading();
});
try {
  await page.route('**/__tile-scheduler-test', route => route.fulfill({contentType:'text/html', body:
    '<link rel="stylesheet" href="/node_modules/leaflet/dist/leaflet.css"><div id="map" style="width:1000px;height:750px"></div>'}));
  await page.goto(`${url}/__tile-scheduler-test`);
  const source = await (await page.request.get(`${url}/src/utils/phaseLayerCache.ts`)).text();
  const leaflet = source.match(/from\s+["']([^"']*leaflet[^"']*)["']/)?.[1];
  assert.ok(leaflet);
  await page.evaluate(async leaflet => {
    const {default:L} = await import(leaflet);
    const {PhaseLayerCache} = await import('/src/utils/phaseLayerCache.ts');
    const map = L.map('map',{zoomAnimation:false,fadeAnimation:false,zoomSnap:.25}).setView([38,115],8);
    const phases = [0,1].map(i=>({id:`phase-${i}`,region_id:'130100',version:`v${i}`,available:true,
      scenes:[{id:`scene-${i}`,available:true}],bounds:null}));
    const h = window.h = {map,phases,events:[],starts:[],active:0,maxActive:0,aborted:0};
    const canvas=document.createElement('canvas');canvas.width=canvas.height=256;
    canvas.getContext('2d').fillRect(0,0,256,256);
    const blob=await new Promise(resolve=>canvas.toBlob(resolve));
    const original=window.fetch.bind(window);
    window.fetch=(input,options={})=>{
      if(typeof input!=='string'||!input.includes('/mosaic/'))return original(input,options);
      const parts=new URL(input,location.href).pathname.split('/');
      const z=Number(parts.at(-3)),x=Number(parts.at(-2)),y=Number(parts.at(-1).split('.')[0]);
      const layer=h.cache.active.layer,bounds=layer._getTiledPixelBounds(map.getCenter()),center=bounds.getCenter();
      h.starts.push({url:input,z,x,y,at:performance.now(),visible:x*256<bounds.max.x&&(x+1)*256>bounds.min.x&&y*256<bounds.max.y&&(y+1)*256>bounds.min.y,
        distance:(x*256+128-center.x)**2+(y*256+128-center.y)**2});
      h.active++;h.maxActive=Math.max(h.maxActive,h.active);
      return new Promise((resolve,reject)=>{
        let ended=false;
        const finish=(abort=false)=>{
          if(ended)return;ended=true;clearTimeout(timer);options.signal?.removeEventListener('abort',cancel);h.active--;
          if(abort){h.aborted++;reject(new DOMException('Aborted','AbortError'))}else resolve(new Response(blob,{status:200}));
        };
        const cancel=()=>finish(true),timer=setTimeout(()=>finish(),160);
        options.signal?.addEventListener('abort',cancel,{once:true});
        if(options.signal?.aborted)cancel();
      });
    };
    h.cache=new PhaseLayerCache(map,state=>h.events.push(state));h.cache.activate(phases[0]);
  },leaflet);
  await ready();
  await page.evaluate(()=>{window.h.starts=[]});
  // Requests continue through intermediate views; obsolete work is cancelled.
  for(const z of [9,10,11,10,9,10]){
    await page.evaluate(z=>window.h.map.setZoom(z),z);await page.waitForTimeout(45);
  }
  for(let i=0;i<4;i++){
    await page.evaluate(()=>window.h.map.panBy([280,70],{animate:false}));await page.waitForTimeout(45);
  }
  const beforeSettle=await page.evaluate(()=>window.h.starts.length);
  assert.ok(beforeSettle>0,'requests start during intermediate zoom/pan views');
  const stopped=await page.evaluate(()=>performance.now());
  await ready();
  const settled=await page.evaluate(()=>({starts:window.h.starts,maxActive:window.h.maxActive}));
  assert.ok(settled.starts.length>4);
  assert.ok(settled.starts.every(r=>r.visible),'only visible detail tiles are dispatched');
  assert.ok(settled.starts[0].at<stopped,'loading starts before movement finishes');
  assert.ok(settled.maxActive<=4,'shared concurrency cap');
  const first=settled.starts.slice(0,4).map(r=>r.distance);
  assert.deepEqual(first,[...first].sort((a,b)=>a-b),'nearest tiles start first');
  // Switching phase during movement loads immediately; the hidden phase stops work.
  await page.evaluate(()=>{const h=window.h;h.starts=[];h.map.fire('movestart');h.cache.activate(h.phases[1]);});
  await page.waitForTimeout(350);
  assert.ok(await page.evaluate(()=>window.h.starts.length)>0,'phase loads while the pointer is still moving');
  await page.evaluate(()=>window.h.map.fire('moveend'));await ready();
  assert.ok(await page.evaluate(()=>window.h.starts.every(r=>r.url.includes('v=v1'))));
  for(const zoom of [10.25,10.5,10.75]){
    await page.evaluate(z=>window.h.map.setZoom(z),zoom);await ready();
  }
  await page.evaluate(()=>{const h=window.h;h.map.panBy([1000,0],{animate:false});});
  await page.waitForTimeout(230);
  await page.evaluate(()=>window.h.cache.suspend());
  const suspended=await page.evaluate(()=>window.h.starts.length);
  await page.waitForTimeout(350);
  assert.equal(await page.evaluate(()=>window.h.starts.length),suspended,'suspend clears queued work');
  await page.evaluate(()=>window.h.cache.resume());await ready();
  const report=await page.evaluate(()=>({passed:true,maxConcurrent:window.h.maxActive,
    finalViewportRequests:window.h.starts.length,aborted:window.h.aborted,errors:window.h.events.filter(e=>e.error)}));
  assert.deepEqual(report.errors,[]);assert.deepEqual(errors,[]);
  await page.evaluate(()=>{window.h.cache.dispose();window.h.map.remove()});
  const output=path.resolve(process.env.BROWSER_OUTPUT_DIR||'data/validation/tile-scheduler');
  await fs.mkdir(output,{recursive:true});await fs.writeFile(path.join(output,'report.json'),JSON.stringify({...report,rapidViewRequests:settled.starts.length,intermediateRequests:beforeSettle},null,2));
  console.log(JSON.stringify({...report,rapidViewRequests:settled.starts.length,intermediateRequests:beforeSettle}));
} finally {await browser.close()}
