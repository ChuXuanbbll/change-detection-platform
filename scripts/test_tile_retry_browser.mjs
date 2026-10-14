import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import {createRequire} from 'node:module';
const require=createRequire(import.meta.url);
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const base=process.env.FRONTEND_URL||'http://localhost:5173';
const browser=await chromium.launch({channel:'chrome',headless:true});
const page=await browser.newPage();
const errors=[];page.on('pageerror',e=>errors.push(e.message));
try {
  await page.route('**/__tile-retry-test',route=>route.fulfill({contentType:'text/html',body:
    '<link rel="stylesheet" href="/node_modules/leaflet/dist/leaflet.css"><div id="map" style="width:500px;height:350px"></div>'}));
  await page.goto(`${base}/__tile-retry-test`);
  const src=await(await page.request.get(`${base}/src/utils/phaseLayerCache.ts`)).text();
  const leaflet=src.match(/from\s+["']([^"']*leaflet[^"']*)["']/)?.[1];assert.ok(leaflet);
  await page.evaluate(async leaflet=>{
    const {default:L}=await import(leaflet);
    const {PhaseLayerCache}=await import('/src/utils/phaseLayerCache.ts');
    const map=L.map('map',{zoomAnimation:false,fadeAnimation:false}).setView([38,115],8);
    const phases=[0,1,2,3].map(i=>({id:`p${i}`,region_id:'130100',version:`v${i}`,available:true,scenes:[{id:`s${i}`,available:true}],bounds:null}));
    const h=window.h={map,phases,mode:'transient',calls:{},active:0,maximum:0,events:[]};
    const canvas=document.createElement('canvas');canvas.width=canvas.height=256;canvas.getContext('2d').fillRect(0,0,256,256);
    const blob=await new Promise(resolve=>canvas.toBlob(resolve));
    const original=window.fetch.bind(window);
    window.fetch=(url,options={})=>{
      if(typeof url!=='string'||!url.includes('/mosaic/'))return original(url,options);
      const calls=h.calls[url]??=[];calls.push(performance.now());h.active++;h.maximum=Math.max(h.maximum,h.active);
      const mode=h.mode,attempt=calls.length;
      return new Promise((resolve,reject)=>{
        let done=false;
        const finish=(abort=false)=>{
          if(done)return;done=true;clearTimeout(timer);options.signal?.removeEventListener('abort',cancel);h.active--;
          if(abort){reject(new DOMException('aborted','AbortError'));return;}
          if(mode==='stale'){resolve(new Response('stale',{status:409}));return;}
          if(mode==='permanent'||(mode==='transient'&&attempt<=2)){
            resolve(new Response('busy',{status:attempt===2?429:503,headers:attempt===1?{'Retry-After':'1'}:{}}));return;
          }
          resolve(new Response(blob,{status:200}));
        };
        const cancel=()=>finish(true),timer=setTimeout(()=>finish(),25);
        options.signal?.addEventListener('abort',cancel,{once:true});if(options.signal?.aborted)cancel();
      });
    };
    h.cache=new PhaseLayerCache(map,state=>h.events.push(state));h.cache.activate(phases[0]);
  },leaflet);
  await page.waitForFunction(()=>{const h=window.h,l=h.cache.active.layer;return h.cache.scheduler.ready&&!l.isLoading()&&Object.values(l._tiles).filter(t=>t.current).every(t=>t.el.naturalWidth===256)});
  const transient=await page.evaluate(()=>({calls:Object.values(window.h.calls),maximum:window.h.maximum,errors:window.h.events.filter(e=>e.error)}));
  assert.ok(transient.calls.length>0);assert.ok(transient.calls.every(c=>c.length===3&&c[1]-c[0]>=990));
  assert.ok(transient.maximum<=4);assert.deepEqual(transient.errors,[]);
  await page.evaluate(()=>{const h=window.h;h.mode='permanent';h.calls={};h.cache.activate(h.phases[1]);});
  await page.waitForFunction(()=>{const l=window.h.cache.active.layer;return l.failed&&!l.isLoading()});
  const permanent=await page.evaluate(()=>Object.values(window.h.calls).map(c=>c.length));
  assert.ok(permanent.every(n=>n===5),'bounded to initial request plus four retries');
  await page.evaluate(()=>{const h=window.h;h.calls={};h.cache.activate(h.phases[2]);});
  await page.waitForFunction(()=>Object.keys(window.h.calls).length>0&&window.h.active===0);
  await page.evaluate(()=>window.h.cache.suspend());
  const cancelled=await page.evaluate(()=>JSON.stringify(window.h.calls));
  await page.waitForTimeout(1300);
  assert.equal(await page.evaluate(()=>JSON.stringify(window.h.calls)),cancelled,'suspend clears backoff timers');
  await page.evaluate(()=>{const h=window.h;h.calls={};h.mode='stale';h.cache.activate(h.phases[3]);h.cache.resume();});
  await page.waitForFunction(()=>window.h.cache.active.layer.stale);
  await page.waitForTimeout(350);
  assert.ok(await page.evaluate(()=>Object.values(window.h.calls).every(c=>c.length===1)),'409 is not retried');
  await page.evaluate(()=>{window.h.cache.dispose();window.h.map.remove()});
  assert.deepEqual(errors,[]);
  const report={passed:true,maximum:transient.maximum,transientTiles:transient.calls.length,maxAttempts:Math.max(...permanent),cancelledRetries:true,staleNotRetried:true};
  const out=path.resolve(process.env.BROWSER_OUTPUT_DIR||'data/validation/tile-retry');await fs.mkdir(out,{recursive:true});
  await fs.writeFile(path.join(out,'report.json'),JSON.stringify(report,null,2));console.log(JSON.stringify(report));
}finally{await browser.close()}
