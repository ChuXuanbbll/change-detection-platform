import fs from 'node:fs/promises';
import {PresentationFile,FileBlob} from '@oai/artifact-tool';
const out='../output/遥感变化检测平台_组会汇报_20261017';
const p=await PresentationFile.importPptx(await FileBlob.load(out+'.pptx'));
for(let i=0;i<p.slides.items.length;i++) {const b=await p.export({slide:p.slides.items[i],format:'png',scale:1.5});await fs.writeFile(`final-${String(i+1).padStart(2,'0')}.png`,new Uint8Array(await b.arrayBuffer()));}

