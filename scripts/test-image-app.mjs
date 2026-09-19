import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { webcrypto } from 'node:crypto';

const html = readFileSync(new URL('../internal/httpx/image_app.html', import.meta.url), 'utf8');
const source = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const result = { content: [{ type:'image', mimeType:'image/png', data:'AQID' }] };
const settle = () => new Promise(resolve => setTimeout(resolve, 20));
function mount(api = {}) {
  const events = {}, elements = {}, messages = [];
  for (const id of ['preview','status','share']) elements[id] = { hidden:false, disabled:false, textContent:'', addEventListener(name, cb) { this[name]=cb; } };
  const parent = { postMessage(message) { messages.push(message); } };
  const win = { parent, openai:api, addEventListener(name,cb) { events[name]=cb; } };
  vm.runInNewContext(source, { window:win, document:{ getElementById:id=>elements[id] }, atob, Uint8Array, File, crypto:webcrypto, console, setTimeout, clearTimeout });
  return { elements, api, messages, globals(){events['openai:set_globals']();}, receive(value, sender=parent) { events.message({source:sender,data:{jsonrpc:'2.0',method:'ui/notifications/tool-result',params:value}}); } };
}

test('preserves pixels, uploads only on selection and includes real file ID before follow-up', async () => {
  const calls=[];
  const ui=mount({uploadFile:async file=>{ calls.push(['upload',Array.from(new Uint8Array(await file.arrayBuffer())),file.type]); return {fileId:'file-real'}; }, setWidgetState:state=>calls.push(['state',state]),sendFollowUpMessage:async()=>calls.push(['follow'])});
  ui.receive(result); await settle();
  assert.equal(calls.length,0);
  assert.equal(ui.elements.preview.src,'data:image/png;base64,AQID');
  await ui.elements.share.click();
  assert.deepEqual(calls[0],['upload',[1,2,3],'image/png']);
  assert.deepEqual(Array.from(calls[1][1].imageIds),['file-real']);
  assert.equal(calls[2][0],'follow');
  await ui.elements.share.click();
  assert.equal(calls.filter(c=>c[0]==='upload').length,1);
});

test('host envelope can hydrate initial image, without automatically uploading it', async () => {
  const ui=mount({toolResponseMetadata:{mcp_tool_result:result}}); await settle();
  assert.equal(ui.elements.preview.src,'data:image/png;base64,AQID');
  assert.equal(ui.elements.share.disabled,true);
});

test('untrusted frames, errors and active content are not images', async () => {
  const ui=mount(); ui.receive(result,{}); await settle();
  assert.equal(ui.elements.preview.src,undefined);
  ui.receive({isError:true,...result}); await settle();
  assert.equal(ui.elements.preview.src,undefined);
  ui.receive({content:[{type:'image',mimeType:'image/svg+xml',data:'AQID'}]}); await settle();
  assert.equal(ui.elements.preview.src,undefined);
});

test('upload errors do not claim success or send a follow-up', async () => {
  let sent=false;
  const ui=mount({uploadFile:async()=>{throw Error('private upstream detail');},setWidgetState:()=>{},sendFollowUpMessage:async()=>{sent=true;}});
  ui.receive(result); await settle(); await ui.elements.share.click();
  assert.equal(sent,false); assert.match(ui.elements.status.textContent,/失败/);
  assert.ok(!ui.elements.status.textContent.includes('private upstream'));
  assert.equal(ui.elements.share.disabled,false);
});

test('concurrent clicks cannot duplicate uploads and no fake file IDs are accepted', async () => {
  let count=0,finish;
  const ui=mount({uploadFile:()=>{count++;return new Promise(resolve=>finish=resolve);},setWidgetState:()=>{throw Error('must not store absent file');}});
  ui.receive(result); await settle(); const first=ui.elements.share.click(); await ui.elements.share.click();
  assert.equal(count,1); finish({}); await first;
  assert.match(ui.elements.status.textContent,/失败/);
});

test('host globals updates during upload do not discard the selected image', async () => {
  let finish,state;
  const ui=mount({toolResponseMetadata:{mcp_tool_result:result},uploadFile:()=>new Promise(resolve=>finish=resolve),setWidgetState:value=>{state=value;}});
  await settle(); const action=ui.elements.share.click(); ui.globals(); await settle();
  finish({fileId:'file-real'}); await action;
  assert.deepEqual(Array.from(state.imageIds),['file-real']);
});

test('switching images during upload never attaches stale pixels', async () => {
  let finish,stored=false;
  const ui=mount({uploadFile:()=>new Promise(resolve=>finish=resolve),setWidgetState:()=>{stored=true;}});
  ui.receive(result); await settle(); const action=ui.elements.share.click();
  ui.receive({content:[{type:'image',mimeType:'image/png',data:'BAUG'}]}); await settle();
  finish({fileId:'file-old'}); await action;
  assert.equal(stored,false);
  assert.match(ui.elements.status.textContent,/将这张图片/);
});
